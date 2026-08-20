/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package keenetic

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Arbuzov/keenetic-operator/internal/metrics"
)

// Client выполняет команды CLI на Keenetic по SSH.
type Client struct {
	Host     string // host:port, напр. 192.168.99.1:22
	User     string
	Password string
	// HostKeyFingerprint — SHA256-фингерпринт хост-ключа роутера в формате
	// ssh.FingerprintSHA256 ("SHA256:..."). Пусто -> ключ не проверяется
	// (приемлемо для LAN, но в проде задавайте фингерпринт).
	HostKeyFingerprint string

	mu sync.Mutex // сериализуем доступ: правки конфига не потокобезопасны
}

var ipHostLine = regexp.MustCompile(`(?m)^\s*ip host\s+(\S+)\s+(\S+)`)
var cdNoise = regexp.MustCompile(`(?i)no such command:\s*cd`)

// hostnamePattern — RFC 1123 subdomain, как и в CRD-валидации spec.hostname.
var hostnamePattern = regexp.MustCompile(
	`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)

// validateHostIP — последний рубеж перед тем, как host/ip попадут строкой
// в интерактивную SSH-сессию роутера. CRD-схема уже ограничивает формат, но
// это не гарантия (прямая правка через API, будущий webhook отключён и т.п.):
// не глядя собирать `ip host <host> <ip>` из непроверенных строк — путь к
// инъекции команд через пробелы/переводы строк в spec.
func validateHostIP(host, ip string) error {
	if !hostnamePattern.MatchString(host) {
		return fmt.Errorf("invalid hostname %q", host)
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is4() {
		return fmt.Errorf("invalid IPv4 address %q", ip)
	}
	return nil
}

// hostKeyCallback пинит ключ роутера, если задан HostKeyFingerprint;
// иначе — небезопасный fallback для LAN.
func (c *Client) hostKeyCallback() ssh.HostKeyCallback {
	if c.HostKeyFingerprint == "" {
		return ssh.InsecureIgnoreHostKey()
	}
	want := c.HostKeyFingerprint
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if got := ssh.FingerprintSHA256(key); got != want {
			return fmt.Errorf("keenetic host key mismatch: got %s, want %s", got, want)
		}
		return nil
	}
}

// run открывает свежую сессию, шлёт строки CLI, возвращает вывод.
func (c *Client) run(ctx context.Context, lines ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Теперь мы блокируемся на чтении до приглашения, так что нужен потолок:
	// молчащий роутер иначе держал бы реконсайл вечно. Отмена доходит до сокета
	// через goroutine ниже.
	ctx, cancel := context.WithTimeout(ctx, sessionTimeout)
	defer cancel()

	cfg := &ssh.ClientConfig{
		User:            c.User,
		Auth:            []ssh.AuthMethod{ssh.Password(c.Password)},
		HostKeyCallback: c.hostKeyCallback(),
	}

	// Timeout живёт на net.Dialer, а не на ssh.ClientConfig: с NewClientConn
	// (вместо ssh.Dial) именно Dialer управляет TCP-соединением и уважает ctx.
	d := net.Dialer{Timeout: 10 * time.Second}
	tcpConn, err := d.DialContext(ctx, "tcp", c.Host)
	if err != nil {
		return "", fmt.Errorf("dial %s: %w", c.Host, err)
	}

	// Ни хендшейк, ни шелл, ни запись/Wait ниже не принимают ctx напрямую,
	// поэтому рвём tcpConn сами при отмене/дедлайне — на всём протяжении run(),
	// а не только на dial. ssh.Client не открывает второй сокет поверх tcpConn,
	// так что закрытие tcpConn корректно разблокирует и хендшейк, и сессию.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = tcpConn.Close()
		case <-done:
		}
	}()

	sshConn, chans, reqs, err := ssh.NewClientConn(tcpConn, c.Host, cfg)
	if err != nil {
		_ = tcpConn.Close()
		return "", fmt.Errorf("ssh handshake %s: %w", c.Host, err)
	}
	conn := ssh.NewClient(sshConn, chans, reqs)
	defer func() { _ = conn.Close() }()

	sess, err := conn.NewSession()
	if err != nil {
		return "", err
	}
	defer func() { _ = sess.Close() }()

	stdin, err := sess.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr strings.Builder
	sess.Stderr = &stderr
	if err := sess.Shell(); err != nil {
		return "", err
	}

	var out strings.Builder

	// Ключевой момент: CLI Keenetic не читает stdin, пока не напечатал
	// приглашение, и всё присланное раньше молча выбрасывает. Слепая запись
	// сразу после Shell() поэтому «удавалась», не делая ничего: и чтение
	// running-config, и запись ip host уходили в никуда, а интерактивный шелл
	// не возвращает кода ошибки, по которому это было бы видно.
	if err := readUntilPrompt(stdout, &out); err != nil {
		return "", fmt.Errorf("waiting for the router prompt: %w", err)
	}
	for _, ln := range lines {
		if _, err := fmt.Fprintln(stdin, ln); err != nil {
			return "", fmt.Errorf("write to router shell: %w", err)
		}
		// Дожидаемся приглашения после каждой команды: это единственный признак,
		// что роутер её дочитал и отработал.
		if err := readUntilPrompt(stdout, &out); err != nil {
			return "", fmt.Errorf("waiting for the router prompt after %q: %w", ln, err)
		}
	}
	if _, err := fmt.Fprintln(stdin, "exit"); err != nil {
		return "", fmt.Errorf("write to router shell: %w", err)
	}
	_ = stdin.Close()
	if waitErr := sess.Wait(); waitErr != nil {
		// Интерактивный шелл не даёт надёжного per-command exit code, так что
		// не проваливаем реконсайл на этом — только видимость в логах.
		log.FromContext(ctx).V(1).Info("router shell exited non-zero", "err", waitErr)
	}

	// Раньше stderr шёл в тот же буфер, что и stdout. Читать их в один
	// strings.Builder теперь нельзя — вывод разбирает наша goroutine, а stderr
	// пишет ssh-библиотека, — но и терять его нельзя: это изменение как раз про
	// то, чтобы отказ роутера было видно. Дописываем после Wait(), когда гонки
	// уже нет.
	if s := stderr.String(); s != "" {
		out.WriteString(s)
	}

	return filterNoise(out.String()), nil
}

// cliPrompt — приглашение Keenetic CLI.
const cliPrompt = "(config)>"

// ansiEscape — управляющие последовательности, которыми роутер перемежает вывод
// (в основном ESC[K при отрисовке эха).
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// promptWindow — сколько хвоста держим для поиска приглашения. Приглашение
// может разорваться между чтениями, а escape-последовательности его удлиняют,
// так что берём с запасом.
const promptWindow = 128

// atPrompt — стоит ли поток ровно на приглашении. Важно, что это конец потока,
// а не «где-то встретилось»: строка `(config)>` может попасться и в выводе
// команды, и тогда мы бы сочли команду завершённой раньше времени, отправили
// следующую и разъехались по парам команда-ответ — то есть вернули бы ровно тот
// класс тихой рассинхронизации, ради которого всё это и написано.
func atPrompt(window string) bool {
	clean := ansiEscape.ReplaceAllString(window, "")
	return strings.HasSuffix(strings.TrimRight(clean, " \t\r\n"), cliPrompt)
}

// sessionTimeout — потолок на всю сессию: логин, все команды и ответы.
// `system configuration save` пишет во флеш и не мгновенен, поэтому запас щедрый.
const sessionTimeout = 60 * time.Second

// readUntilPrompt дочитывает поток до следующего приглашения CLI, складывая
// прочитанное в out. Возвращает ошибку чтения (в том числе EOF), если
// приглашение так и не пришло.
func readUntilPrompt(r io.Reader, out *strings.Builder) error {
	buf := make([]byte, 4096)
	var tail string
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			out.WriteString(chunk)
			combined := tail + chunk
			if atPrompt(combined) {
				return nil
			}
			if len(combined) > promptWindow {
				combined = combined[len(combined)-promptWindow:]
			}
			tail = combined
		}
		if readErr != nil {
			return readErr
		}
	}
}

// filterNoise выкидывает паразитные строки "no such command: cd".
func filterNoise(s string) string {
	var b strings.Builder
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if cdNoise.MatchString(sc.Text()) {
			continue
		}
		b.WriteString(sc.Text())
		b.WriteByte('\n')
	}
	return b.String()
}

// EnsureHost идемпотентно добавляет ip host и сохраняет конфиг.
func (c *Client) EnsureHost(ctx context.Context, host, ip string) (err error) {
	if err = validateHostIP(host, ip); err != nil {
		return err
	}
	// Счётчик заводим только после валидации: кривой spec до SSH не доходит,
	// и алерт на «роутер недоступен» не должен на нём загораться.
	defer metrics.ObserveRouterOp(metrics.OpEnsure, time.Now(), &err)

	// Именно hasHost, не HasHost: экспортированный сам себя измеряет, и через
	// него одна неудачная проверка внутри ensure легла бы сразу в два счётчика.
	ok, err := c.hasHost(ctx, host, ip)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	_, err = c.run(ctx,
		fmt.Sprintf("ip host %s %s", host, ip),
		"system configuration save",
	)
	return err
}

// DeleteHost убирает ip host и сохраняет конфиг.
func (c *Client) DeleteHost(ctx context.Context, host, ip string) (err error) {
	if verr := validateHostIP(host, ip); verr != nil {
		// EnsureHost validates the same input before ever writing to the router,
		// so an invalid host/ip here could never have been applied — nothing to
		// clean up. Returning the error would wedge the finalizer forever.
		log.FromContext(ctx).Info("skipping router cleanup: invalid host/ip in spec", "err", verr)
		return nil
	}
	defer metrics.ObserveRouterOp(metrics.OpDelete, time.Now(), &err)

	_, err = c.run(ctx,
		fmt.Sprintf("no ip host %s %s", host, ip),
		"system configuration save",
	)
	return err
}

// HasHost — есть ли запись в running-config.
func (c *Client) HasHost(ctx context.Context, host, ip string) (_ bool, err error) {
	defer metrics.ObserveRouterOp(metrics.OpHas, time.Now(), &err)

	return c.hasHost(ctx, host, ip)
}

// hasHost — та же проверка без метрики, для вызова изнутри другой измеряемой
// операции.
func (c *Client) hasHost(ctx context.Context, host, ip string) (bool, error) {
	hosts, err := c.listHosts(ctx)
	if err != nil {
		return false, err
	}
	return hosts[strings.ToLower(host)] == ip, nil
}

// CountHosts — число записей ip host (для гарда на 64).
func (c *Client) CountHosts(ctx context.Context) (_ int, err error) {
	defer metrics.ObserveRouterOp(metrics.OpCount, time.Now(), &err)

	hosts, err := c.listHosts(ctx)
	if err != nil {
		return 0, err
	}
	return len(hosts), nil
}

func (c *Client) listHosts(ctx context.Context) (map[string]string, error) {
	out, err := c.run(ctx, "show running-config")
	if err != nil {
		return nil, err
	}
	res := map[string]string{}
	for _, m := range ipHostLine.FindAllStringSubmatch(out, -1) {
		res[strings.ToLower(m[1])] = m[2]
	}
	return res, nil
}

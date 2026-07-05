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
	"net"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"sigs.k8s.io/controller-runtime/pkg/log"
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
	sshConn, chans, reqs, err := ssh.NewClientConn(tcpConn, c.Host, cfg)
	if err != nil {
		_ = tcpConn.Close()
		return "", fmt.Errorf("ssh handshake %s: %w", c.Host, err)
	}
	conn := ssh.NewClient(sshConn, chans, reqs)
	defer func() { _ = conn.Close() }()

	// ctx покрывает только dial выше; хендшейк/шелл/запись — блокирующие
	// вызовы без своего ctx, поэтому рвём соединение сами при отмене/дедлайне,
	// иначе завёрнутый роутер держит мьютекс до истечения TCP-таймаутов ОС.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	sess, err := conn.NewSession()
	if err != nil {
		return "", err
	}
	defer func() { _ = sess.Close() }()

	stdin, _ := sess.StdinPipe()
	var out strings.Builder
	sess.Stdout = &out
	sess.Stderr = &out
	if err := sess.Shell(); err != nil {
		return "", err
	}
	for _, ln := range lines {
		if _, err := fmt.Fprintln(stdin, ln); err != nil {
			return "", fmt.Errorf("write to router shell: %w", err)
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

	return filterNoise(out.String()), nil
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
func (c *Client) EnsureHost(ctx context.Context, host, ip string) error {
	if err := validateHostIP(host, ip); err != nil {
		return err
	}
	ok, err := c.HasHost(ctx, host, ip)
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
func (c *Client) DeleteHost(ctx context.Context, host, ip string) error {
	if err := validateHostIP(host, ip); err != nil {
		// EnsureHost validates the same input before ever writing to the router,
		// so an invalid host/ip here could never have been applied — nothing to
		// clean up. Returning the error would wedge the finalizer forever.
		log.FromContext(ctx).Info("skipping router cleanup: invalid host/ip in spec", "err", err)
		return nil
	}
	_, err := c.run(ctx,
		fmt.Sprintf("no ip host %s %s", host, ip),
		"system configuration save",
	)
	return err
}

// HasHost — есть ли запись в running-config.
func (c *Client) HasHost(ctx context.Context, host, ip string) (bool, error) {
	hosts, err := c.listHosts(ctx)
	if err != nil {
		return false, err
	}
	return hosts[strings.ToLower(host)] == ip, nil
}

// CountHosts — число записей ip host (для гарда на 64).
func (c *Client) CountHosts(ctx context.Context) (int, error) {
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

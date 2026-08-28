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
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Arbuzov/keenetic-operator/internal/metrics"
)

// Proxy — одна запись `ip http proxy` на роутере: публикация внутреннего
// веб-приложения под именем KeenDNS.
type Proxy struct {
	// Name — идентификатор записи в конфиге. Удаление идёт по нему, не по домену.
	Name string

	// Zone — аргумент `domain static`. Это ЗОНА, а не FQDN: опубликованное имя
	// роутер собирает сам как `<Name>.<Zone>`. Записанный сюда полный хост даёт
	// dev.dev.example.keenetic.link — имя, которого нет, после чего роутер
	// отвечает на настоящее имя своим веб-интерфейсом. Проверено на живом
	// роутере, стоило простоя всех опубликованных сервисов.
	Zone string
	// NDNS — запись стоит на `domain ndns`: зону подставляет сам роутер из
	// своего имени KeenDNS. Ровно так выглядят записи, заведённые через
	// веб-интерфейс, и только они публикуются в KeenDNS.
	NDNS bool

	// Scheme — http | https (первый аргумент `upstream`).
	Scheme string
	// Address — IPv4 или MAC. Роутер принимает оба (`upstream http
	// e4:5f:01:1e:16:46 8123` — живая запись), поэтому парсер не сужает.
	Address string
	Port    int32

	// SecurityLevel — public | private. Пусто, если роутер строку не напечатал.
	SecurityLevel string

	// Заголовки и редирект, как у записей из веб-интерфейса. PreserveHost —
	// не косметика: без него роутер уходит на upstream с `Host: <ip>`, ни одно
	// правило ingress-контроллера не совпадает, и публикация отвечает 404.
	SSLRedirect     bool
	XRealIP         bool
	PreserveHost    bool
	PreserveReferer bool
	PreserveOrigin  bool

	// Auth / AuthSet — `auth` / `no auth`. AuthSet различает «роутер сказал
	// no auth» и «роутер про auth не написал вовсе»; без этого отсутствие
	// строки читалось бы как явный false и гоняло бы нас на перезапись.
	Auth    bool
	AuthSet bool
}

// ZoneOf — родительская зона FQDN: то, что роутер ждёт в `domain static`.
// Для notes.example.keenetic.link это example.keenetic.link.
func ZoneOf(fqdn string) string {
	_, zone, found := strings.Cut(fqdn, ".")
	if !found {
		return ""
	}
	return zone
}

// proxyNamePattern — что допустимо в `ip http proxy <name>`. Как и
// validateHostIP, это последний рубеж перед склейкой строки для интерактивной
// SSH-сессии: пробел или перевод строки в имени — это инъекция команды.
var proxyNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)

// ValidProxyName — годится ли строка в качестве имени записи на роутере.
// Экспортируется, чтобы source-контроллер отсеивал негодные имена ДО создания
// CR: иначе объект родился бы, не прошёл валидацию CRD-схемы и реконсайл
// крутился бы на ошибке создания, вместо того чтобы один раз сказать почему.
func ValidProxyName(name string) bool { return proxyNamePattern.MatchString(name) }

var (
	proxyHeaderLine = regexp.MustCompile(`^ip http proxy\s+(\S+)\s*(.*)$`)
	upstreamLine    = regexp.MustCompile(`^upstream\s+(https?)\s+(\S+)\s+(\d{1,5})$`)
	domainStaticRe  = regexp.MustCompile(`^domain\s+static\s+(\S+)$`)
	securityLevelRe = regexp.MustCompile(`^security-level\s+(public|private)$`)
	// routerRefusal — как роутер сообщает, что команду не понял. Проверяем это
	// явно: интерактивный шелл не даёт кода возврата, так что отвергнутая
	// команда иначе выглядит как успешная запись, и оператор рапортует Ready
	// на несуществующей записи.
	routerRefusal = regexp.MustCompile(`(?i)(no such command|invalid (argument|value)|syntax error|unknown command)`)
)

// validateProxy — проверка перед тем, как поля уедут строкой в CLI роутера.
// CRD-схема ограничивает то же самое, но она не гарантия: CR можно создать
// мимо неё (прямой apply старой версией схемы, будущий отключённый webhook).
func validateProxy(p Proxy) error {
	if !proxyNamePattern.MatchString(p.Name) {
		return fmt.Errorf("invalid proxy name %q", p.Name)
	}
	// Зона обязательна только для `domain static`: при ndns её подставляет
	// роутер, и слать туда своё значение нечем и незачем.
	if !p.NDNS && !hostnamePattern.MatchString(p.Zone) {
		return fmt.Errorf("invalid domain zone %q", p.Zone)
	}
	addr, err := netip.ParseAddr(p.Address)
	if err != nil || !addr.Is4() {
		return fmt.Errorf("invalid IPv4 upstream address %q", p.Address)
	}
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("invalid upstream port %d", p.Port)
	}
	if p.Scheme != "http" && p.Scheme != "https" {
		return fmt.Errorf("invalid upstream scheme %q", p.Scheme)
	}
	if p.SecurityLevel != "public" && p.SecurityLevel != "private" {
		return fmt.Errorf("invalid security level %q", p.SecurityLevel)
	}
	return nil
}

// parseProxies разбирает `show running-config` в записи по именам.
//
// Секция печатается вложенным блоком:
//
//	ip http proxy notes
//	    domain static notes.example.keenetic.link
//	    upstream http 192.168.99.44 80
//	    security-level public
//	!
//
// Плоскую форму (`ip http proxy notes upstream http ...`) тоже принимаем: в
// командном справочнике Keenetic примеры напечатаны именно так, а стоит это
// ничего — хвост заголовка разбирает тот же код, что и строки блока.
func parseProxies(runningConfig string) map[string]Proxy {
	res := map[string]Proxy{}
	var cur *Proxy

	flush := func() {
		if cur != nil {
			res[cur.Name] = *cur
			cur = nil
		}
	}

	sc := bufio.NewScanner(strings.NewReader(runningConfig))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := proxyHeaderLine.FindStringSubmatch(line); m != nil {
			flush()
			cur = &Proxy{Name: m[1]}
			if rest := strings.TrimSpace(m[2]); rest != "" {
				applyProxyLine(cur, rest)
			}
			continue
		}
		if cur == nil {
			continue
		}
		if line == "!" || line == "" {
			flush()
			continue
		}
		if !applyProxyLine(cur, line) {
			// Строка не из этого блока — значит блок кончился. Собственный
			// заголовок мы бы поймали выше, так что терять тут нечего.
			flush()
		}
	}
	flush()
	return res
}

// applyProxyLine применяет одну строку блока. false — строка не наша.
func applyProxyLine(p *Proxy, line string) bool {
	switch line {
	case "domain ndns":
		p.NDNS, p.Zone = true, ""
		return true
	case "ssl redirect":
		p.SSLRedirect = true
		return true
	case "x-real-ip":
		p.XRealIP = true
		return true
	case "preserve-host":
		p.PreserveHost = true
		return true
	case "preserve-referer":
		p.PreserveReferer = true
		return true
	case "preserve-origin":
		p.PreserveOrigin = true
		return true
	case "auth":
		p.Auth, p.AuthSet = true, true
		return true
	case "no auth":
		p.Auth, p.AuthSet = false, true
		return true
	}
	if m := domainStaticRe.FindStringSubmatch(line); m != nil {
		p.Zone, p.NDNS = m[1], false
		return true
	}
	if m := upstreamLine.FindStringSubmatch(line); m != nil {
		port, err := strconv.ParseInt(m[3], 10, 32)
		if err != nil {
			return false
		}
		p.Scheme, p.Address, p.Port = m[1], m[2], int32(port)
		return true
	}
	if m := securityLevelRe.FindStringSubmatch(line); m != nil {
		p.SecurityLevel = m[1]
		return true
	}
	return false
}

// proxySatisfied — совпадает ли запись на роутере с желаемой настолько, чтобы
// не трогать конфиг.
//
// Сравнение намеренно несимметричное: поля, которых роутер не напечатал,
// считаются устраивающими. Причина не в снисходительности, а в том, что каждая
// перезапись — это `system configuration save`, то есть запись во флеш, и
// повторяется она раз в 5 минут вечно. Поле, которое роутер хранит, но не
// показывает, при строгом сравнении не сошлось бы никогда — получился бы
// незатухающий цикл записи во флеш на ровном месте. Записи, которой на роутере
// нет вовсе, это не касается: там путь «создать целиком».
func proxySatisfied(cur, want Proxy) bool {
	if !domainSatisfied(cur, want) {
		return false
	}
	if cur.Scheme != want.Scheme || cur.Address != want.Address || cur.Port != want.Port {
		return false
	}
	if cur.SecurityLevel != "" && cur.SecurityLevel != want.SecurityLevel {
		return false
	}
	if cur.AuthSet && cur.Auth != want.Auth {
		return false
	}
	// Флаги сравниваем в одну сторону: нам важно, что нужное включено, а не
	// что лишнего нет. Роутер печатает их, только когда они включены, так что
	// «не хватает» отличимо от «не сказано», и цикла перезаписи это не даёт.
	if want.SSLRedirect && !cur.SSLRedirect {
		return false
	}
	if want.XRealIP && !cur.XRealIP {
		return false
	}
	if want.PreserveHost && !cur.PreserveHost {
		return false
	}
	if want.PreserveReferer && !cur.PreserveReferer {
		return false
	}
	if want.PreserveOrigin && !cur.PreserveOrigin {
		return false
	}
	return true
}

// domainSatisfied — совпадает ли способ задания домена.
//
// Сравнение строгое, в отличие от остальных полей, и это осознанно: `ndns` и
// `static` — не два способа записать одно и то же. Только `ndns` публикует имя
// в KeenDNS; запись со static-зоной внутри keenetic.link просто не заявляет
// имя, и роутер отвечает на него собственным веб-интерфейсом, ничем не
// сигналя об ошибке. Считать их взаимозаменяемыми — ровно та ошибка, которая
// положила все опубликованные сервисы.
func domainSatisfied(cur, want Proxy) bool {
	if want.NDNS {
		return cur.NDNS
	}
	return !cur.NDNS && strings.EqualFold(cur.Zone, want.Zone)
}

// proxyCommands — команды, приводящие запись к желаемому виду.
//
// `ip http proxy <name>` переводит CLI во вложенный контекст, остальные строки
// идут уже в нём, `exit` возвращает наверх. Поэтому же readUntilPrompt обязан
// узнавать приглашения вида `(config-...)>` — иначе сессия висела бы до
// таймаута на первой же вложенной команде.
func proxyCommands(p Proxy) []string {
	domain := "domain ndns"
	if !p.NDNS {
		domain = fmt.Sprintf("domain static %s", p.Zone)
	}

	cmds := []string{
		fmt.Sprintf("ip http proxy %s", p.Name),
		domain,
		fmt.Sprintf("upstream %s %s %d", p.Scheme, p.Address, p.Port),
		fmt.Sprintf("security-level %s", p.SecurityLevel),
	}
	// Флаги — только включение. Выключать то, что кто-то поставил руками,
	// оператору незачем: он владеет адресом и доменом записи, а не всей её
	// настройкой.
	for _, f := range []struct {
		on  bool
		cmd string
	}{
		{p.SSLRedirect, "ssl redirect"},
		{p.XRealIP, "x-real-ip"},
		{p.PreserveHost, "preserve-host"},
		{p.PreserveReferer, "preserve-referer"},
		{p.PreserveOrigin, "preserve-origin"},
	} {
		if f.on {
			cmds = append(cmds, f.cmd)
		}
	}
	if p.Auth {
		cmds = append(cmds, "auth")
	}
	return append(cmds, "exit")
}

// EnsureProxy идемпотентно приводит публикацию к желаемому виду и сохраняет конфиг.
func (c *Client) EnsureProxy(ctx context.Context, p Proxy) (err error) {
	if err = validateProxy(p); err != nil {
		return err
	}
	// Счётчик заводим только после валидации: кривой spec до роутера не доходит,
	// и алерт на «роутер недоступен» не должен на нём загораться.
	defer metrics.ObserveRouterOp(metrics.OpEnsureProxy, time.Now(), &err)

	proxies, err := c.listProxies(ctx)
	if err != nil {
		return err
	}
	if cur, ok := proxies[p.Name]; ok && proxySatisfied(cur, p) {
		return nil
	}

	out, err := c.run(ctx, append(proxyCommands(p), "system configuration save")...)
	if err != nil {
		return err
	}
	return refusalError(out)
}

// DeleteProxy убирает публикацию и сохраняет конфиг.
func (c *Client) DeleteProxy(ctx context.Context, name string) (err error) {
	if !proxyNamePattern.MatchString(name) {
		// Записать такое имя EnsureProxy не мог — валидация та же, — значит и
		// убирать нечего. Ошибка здесь навсегда заклинила бы finalizer.
		log.FromContext(ctx).Info("пропускаем уборку прокси: некорректное имя в spec", "name", name)
		return nil
	}
	defer metrics.ObserveRouterOp(metrics.OpDeleteProxy, time.Now(), &err)

	out, err := c.run(ctx,
		fmt.Sprintf("no ip http proxy %s", name),
		"system configuration save",
	)
	if err != nil {
		return err
	}
	return refusalError(out)
}

// GetProxy — запись с таким именем на роутере, если она есть.
func (c *Client) GetProxy(ctx context.Context, name string) (_ Proxy, _ bool, err error) {
	defer metrics.ObserveRouterOp(metrics.OpGetProxy, time.Now(), &err)

	proxies, err := c.listProxies(ctx)
	if err != nil {
		return Proxy{}, false, err
	}
	p, ok := proxies[name]
	return p, ok, nil
}

// CountProxies — число записей `ip http proxy` на роутере.
func (c *Client) CountProxies(ctx context.Context) (_ int, err error) {
	defer metrics.ObserveRouterOp(metrics.OpGetProxy, time.Now(), &err)

	proxies, err := c.listProxies(ctx)
	if err != nil {
		return 0, err
	}
	return len(proxies), nil
}

// listProxies — все записи `ip http proxy` из running-config. Без метрики:
// вызывается изнутри уже измеряемых операций.
func (c *Client) listProxies(ctx context.Context) (map[string]Proxy, error) {
	out, err := c.run(ctx, "show running-config")
	if err != nil {
		return nil, err
	}
	proxies := parseProxies(out)
	metrics.RouterWebApps.Set(float64(len(proxies)))
	return proxies, nil
}

// refusalError превращает отказ роутера в ошибку. Интерактивный шелл не даёт
// кода возврата на команду, так что без этого отвергнутая команда неотличима от
// применённой — и статус CR рапортовал бы Ready на том, чего на роутере нет.
func refusalError(out string) error {
	for _, line := range strings.Split(out, "\n") {
		if routerRefusal.MatchString(line) {
			return fmt.Errorf("router rejected the command: %s", strings.TrimSpace(line))
		}
	}
	return nil
}

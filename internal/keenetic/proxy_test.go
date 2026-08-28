/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package keenetic

import (
	"reflect"
	"strings"
	"testing"
)

// Как секция выглядит в running-config: вложенный блок, закрытый `!`, рядом с
// чужими строками верхнего уровня.
const runningConfigWithProxies = `
ip host dev.example.keenetic.link 192.168.99.1
ip host notes.example.keenetic.link 192.168.99.1
ip http proxy notes
    domain static example.keenetic.link
    upstream http 192.168.99.44 80
    ssl redirect
    security-level public
    preserve-host
    no auth
!
ip http proxy k8s
    domain ndns
    upstream https 192.168.99.44 6443
    ssl redirect
    security-level private
    x-real-ip
    auth
!
ip dhcp pool _WEBADMIN
    range 192.168.99.20 192.168.99.200
!
`

func TestParseProxiesReadsANestedBlock(t *testing.T) {
	got := parseProxies(runningConfigWithProxies)

	want := map[string]Proxy{
		"notes": {
			Name: "notes", Zone: "example.keenetic.link",
			Scheme: "http", Address: "192.168.99.44", Port: 80,
			SecurityLevel: "public", Auth: false, AuthSet: true,
			SSLRedirect: true, PreserveHost: true,
		},
		"k8s": {
			Name: "k8s", NDNS: true,
			Scheme: "https", Address: "192.168.99.44", Port: 6443,
			SecurityLevel: "private", Auth: true, AuthSet: true,
			SSLRedirect: true, XRealIP: true,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseProxies() = %#v, want %#v", got, want)
	}
}

// Строки соседних секций не должны затекать в блок: `ip dhcp pool` идёт сразу
// за проксями, и если бы блок не закрывался, его `range` попал бы в запись.
func TestParseProxiesDoesNotAbsorbTheNextSection(t *testing.T) {
	got := parseProxies(runningConfigWithProxies)
	if len(got) != 2 {
		t.Fatalf("parsed %d entries, want 2: %#v", len(got), got)
	}
	if got["notes"].Address != "192.168.99.44" {
		t.Errorf("notes upstream = %q, want 192.168.99.44", got["notes"].Address)
	}
}

// В командном справочнике Keenetic примеры напечатаны одной строкой. Мы так не
// пишем, но прочитать обязаны — иначе запись, заведённая руками по мануалу,
// выглядела бы отсутствующей и оператор создавал бы её поверх.
func TestParseProxiesReadsTheFlatForm(t *testing.T) {
	got := parseProxies("ip http proxy emby upstream http 192.168.1.100 80\n")

	if p := got["emby"]; p.Address != "192.168.1.100" || p.Port != 80 || p.Scheme != "http" {
		t.Errorf("flat form parsed as %#v", p)
	}
}

func TestProxySatisfied(t *testing.T) {
	want := Proxy{
		Name: "notes", NDNS: true,
		Scheme: "http", Address: "192.168.99.44", Port: 80,
		SecurityLevel: "public",
	}

	tests := []struct {
		name string
		cur  Proxy
		ok   bool
	}{
		{
			name: "точное совпадение",
			cur:  want,
			ok:   true,
		},
		{
			// Главный случай. `domain static` и `domain ndns` — не два способа
			// записать одно и то же: только ndns заявляет имя в KeenDNS, а
			// static-запись внутри keenetic.link молча отдаёт имя роутеру, и он
			// отвечает своим веб-интерфейсом. Считать их равными — та самая
			// ошибка, которая положила все публикации.
			name: "на роутере domain static, а нужен ndns",
			cur: Proxy{
				Name: "notes", Zone: "example.keenetic.link",
				Scheme: "http", Address: "192.168.99.44", Port: 80,
				SecurityLevel: "public",
			},
			ok: false,
		},
		{
			name: "другой upstream",
			cur: Proxy{
				Name: "notes", NDNS: true,
				Scheme: "http", Address: "192.168.99.45", Port: 80,
				SecurityLevel: "public",
			},
			ok: false,
		},
		{
			name: "другой порт",
			cur: Proxy{
				Name: "notes", NDNS: true,
				Scheme: "http", Address: "192.168.99.44", Port: 8080,
				SecurityLevel: "public",
			},
			ok: false,
		},
		{
			// Роутер молчит про поле -> сравнивать нечего. Строгое сравнение
			// здесь дало бы вечную перезапись.
			name: "роутер не напечатал security-level",
			cur: Proxy{
				Name: "notes", NDNS: true,
				Scheme: "http", Address: "192.168.99.44", Port: 80,
			},
			ok: true,
		},
		{
			name: "роутер напечатал другой security-level",
			cur: Proxy{
				Name: "notes", NDNS: true,
				Scheme: "http", Address: "192.168.99.44", Port: 80,
				SecurityLevel: "private",
			},
			ok: false,
		},
		{
			name: "auth включён на роутере, в spec выключен",
			cur: Proxy{
				Name: "notes", NDNS: true,
				Scheme: "http", Address: "192.168.99.44", Port: 80,
				SecurityLevel: "public", Auth: true, AuthSet: true,
			},
			ok: false,
		},
		{
			// Без AuthSet отсутствие строки читалось бы как явный false и
			// совпадало бы с want случайно — а тут want.Auth = false, так что
			// проверяем именно, что отсутствие не считается расхождением.
			name: "роутер про auth не написал",
			cur: Proxy{
				Name: "notes", NDNS: true,
				Scheme: "http", Address: "192.168.99.44", Port: 80,
				SecurityLevel: "public",
			},
			ok: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := proxySatisfied(tt.cur, want); got != tt.ok {
				t.Errorf("proxySatisfied() = %v, want %v", got, tt.ok)
			}
		})
	}
}

// Порядок важен: заголовок уводит CLI во вложенный контекст, exit возвращает
// наверх. Без exit следующая команда сессии (`system configuration save`)
// ушла бы в контекст прокси.
func TestProxyCommandsEnterAndLeaveTheContext(t *testing.T) {
	got := proxyCommands(Proxy{
		Name: "notes", NDNS: true,
		Scheme: "http", Address: "192.168.99.44", Port: 80,
		SecurityLevel: "public",
		SSLRedirect:   true, XRealIP: true,
		PreserveHost: true, PreserveReferer: true, PreserveOrigin: true,
	})

	want := []string{
		"ip http proxy notes",
		"domain ndns",
		"upstream http 192.168.99.44 80",
		"security-level public",
		"ssl redirect",
		"x-real-ip",
		"preserve-host",
		"preserve-referer",
		"preserve-origin",
		"exit",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("proxyCommands() =\n%q\nwant\n%q", got, want)
	}
}

// Поля уезжают в интерактивный шелл строкой, так что перевод строки в них —
// это дописанная команда, а не кривое значение.
func TestValidateProxyRejectsCommandInjection(t *testing.T) {
	base := Proxy{
		Name: "notes", Zone: "example.keenetic.link",
		Scheme: "http", Address: "192.168.99.44", Port: 80,
		SecurityLevel: "public",
	}

	tests := map[string]func(*Proxy){
		"перевод строки в имени": func(p *Proxy) { p.Name = "notes\nno ip host x" },
		"пробел в имени":         func(p *Proxy) { p.Name = "notes x" },
		"перевод строки в зоне":  func(p *Proxy) { p.Zone = "example.link\nreboot" },
		"не IPv4 в upstream":     func(p *Proxy) { p.Address = "192.168.99.44; reboot" },
		"порт вне диапазона":     func(p *Proxy) { p.Port = 70000 },
		"неизвестная схема":      func(p *Proxy) { p.Scheme = "ftp" },
		"неизвестный уровень":    func(p *Proxy) { p.SecurityLevel = "open" },
		"пустой уровень доступа": func(p *Proxy) { p.SecurityLevel = "" },
		"пустое имя":             func(p *Proxy) { p.Name = "" },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			p := base
			mutate(&p)
			if err := validateProxy(p); err == nil {
				t.Errorf("validateProxy(%#v) = nil, want error", p)
			}
		})
	}

	if err := validateProxy(base); err != nil {
		t.Errorf("validateProxy(valid) = %v, want nil", err)
	}
}

// Интерактивный шелл не возвращает код команды: без разбора вывода отвергнутая
// команда неотличима от применённой, и CR уходил бы в Ready на пустом месте.
func TestRefusalErrorSpotsARejectedCommand(t *testing.T) {
	out := strings.Join([]string{
		"(config)> ip http proxy notes",
		"no such command: ip http proxy",
		"(config)> ",
	}, "\n")

	err := refusalError(out)
	if err == nil {
		t.Fatal("refusalError() = nil, want error")
	}
	if !strings.Contains(err.Error(), "no such command") {
		t.Errorf("error does not quote the router: %v", err)
	}
}

func TestRefusalErrorPassesCleanOutput(t *testing.T) {
	if err := refusalError("(config)> ip http proxy notes\n(config-proxy)> \n"); err != nil {
		t.Errorf("refusalError(clean) = %v, want nil", err)
	}
}

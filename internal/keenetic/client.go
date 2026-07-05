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
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Client выполняет команды CLI на Keenetic по SSH.
type Client struct {
	Host     string // host:port, напр. 192.168.99.1:22
	User     string
	Password string

	mu sync.Mutex // сериализуем доступ: правки конфига не потокобезопасны
}

var ipHostLine = regexp.MustCompile(`(?m)^\s*ip host\s+(\S+)\s+(\S+)`)
var cdNoise = regexp.MustCompile(`(?i)no such command:\s*cd`)

// run открывает свежую сессию, шлёт строки CLI, возвращает вывод.
func (c *Client) run(ctx context.Context, lines ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cfg := &ssh.ClientConfig{
		User:            c.User,
		Auth:            []ssh.AuthMethod{ssh.Password(c.Password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // LAN-роутер; в проде пиньте ключ
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
		return "", fmt.Errorf("ssh handshake %s: %w", c.Host, err)
	}
	conn := ssh.NewClient(sshConn, chans, reqs)
	defer func() { _ = conn.Close() }()

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
	_ = sess.Wait()

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

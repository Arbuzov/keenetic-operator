/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package keenetic

import "testing"

func TestFilterNoise(t *testing.T) {
	in := "ip host grafana.example.com 192.168.99.50\nno such command: cd\nip host nas.example.com 192.168.99.44\n"
	want := "ip host grafana.example.com 192.168.99.50\nip host nas.example.com 192.168.99.44\n"

	if got := filterNoise(in); got != want {
		t.Errorf("filterNoise() =\n%q\nwant\n%q", got, want)
	}
}

func TestIPHostLineParsing(t *testing.T) {
	out := "interface Loopback0\n  ip address 1.1.1.1/32\nip host GRAFANA.example.com 192.168.99.50\n  ip host nas.example.com 192.168.99.44\n"

	matches := ipHostLine.FindAllStringSubmatch(out, -1)
	if len(matches) != 2 {
		t.Fatalf("expected 2 ip host matches, got %d: %v", len(matches), matches)
	}
	if matches[0][1] != "GRAFANA.example.com" || matches[0][2] != "192.168.99.50" {
		t.Errorf("unexpected first match: %v", matches[0])
	}
	if matches[1][1] != "nas.example.com" || matches[1][2] != "192.168.99.44" {
		t.Errorf("unexpected second match: %v", matches[1])
	}
}

func TestValidateHostIP(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		ip      string
		wantErr bool
	}{
		{"valid fqdn", "grafana.whitediver.keenetic.link", "192.168.99.50", false},
		{"valid single label", "nas", "192.168.99.44", false},
		{"embedded newline injects a command", "nas\nsystem reboot", "192.168.99.44", true},
		{"embedded space", "nas host", "192.168.99.44", true},
		{"embedded semicolon", "nas;reboot", "192.168.99.44", true},
		{"empty host", "", "192.168.99.44", true},
		{"ipv4 out of range", "nas", "999.999.999.999", true},
		{"not an ip at all", "nas", "not-an-ip", true},
		{"ipv6 rejected", "nas", "::1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHostIP(tc.host, tc.ip)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateHostIP(%q, %q) error = %v, wantErr %v", tc.host, tc.ip, err, tc.wantErr)
			}
		})
	}
}

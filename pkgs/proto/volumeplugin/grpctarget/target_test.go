// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package grpctarget

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"spaces only", "   ", ""},
		{"explicit unix", "unix:///run/cube/plugin.sock", "unix:///run/cube/plugin.sock"},
		{"explicit tcp", "tcp://127.0.0.1:9100", "tcp://127.0.0.1:9100"},
		{"implicit unix", "/run/cube/plugin.sock", "unix:///run/cube/plugin.sock"},
		{"implicit unix trimmed", "  /run/cube/plugin.sock  ", "unix:///run/cube/plugin.sock"},
		{"implicit tcp", "127.0.0.1:9100", "127.0.0.1:9100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.in); got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseListen(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantNetwork string
		wantHost    string
	}{
		{"empty", "", "tcp", ""},
		{"explicit unix", "unix:///run/cube/plugin.sock", "unix", "/run/cube/plugin.sock"},
		{"explicit tcp", "tcp://127.0.0.1:9100", "tcp", "127.0.0.1:9100"},
		{"implicit unix", "/run/cube/plugin.sock", "unix", "/run/cube/plugin.sock"},
		{"implicit tcp", "127.0.0.1:9100", "tcp", "127.0.0.1:9100"},
		{"unix trimmed", "  unix:///run/cube/plugin.sock  ", "unix", "/run/cube/plugin.sock"},
		{"tcp trimmed", "  tcp://127.0.0.1:9100  ", "tcp", "127.0.0.1:9100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			network, host := ParseListen(tc.in)
			if network != tc.wantNetwork || host != tc.wantHost {
				t.Errorf("ParseListen(%q) = (%q, %q), want (%q, %q)", tc.in, network, host, tc.wantNetwork, tc.wantHost)
			}
		})
	}
}

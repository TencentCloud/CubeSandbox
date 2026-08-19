// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package config

import "testing"

// conf builds a Config carrying only the template-center fields under test.
func conf(enabled bool) *Config {
	return &Config{Common: &CommonConf{TemplateCenterEnabled: enabled}}
}

func TestTemplateCenterEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{name: "nil config is disabled", cfg: nil, want: false},
		{name: "nil common is disabled", cfg: &Config{}, want: false},
		// The unset zero value must behave exactly like an explicit false, so a
		// config that never mentions the key stays fully local.
		{name: "unset key is disabled", cfg: conf(false), want: false},
		{name: "explicit false is disabled", cfg: conf(false), want: false},
		{name: "explicit true is enabled", cfg: conf(true), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.TemplateCenterEnabled(); got != tc.want {
				t.Fatalf("TemplateCenterEnabled() = %t, want %t", got, tc.want)
			}
		})
	}
}

// TemplateBuildRemote is a pure alias of the master switch now: builds are
// remote exactly when the switch is on, with no second knob to consult.
func TestTemplateBuildRemoteFollowsSwitch(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{name: "nil config is local", cfg: nil, want: false},
		{name: "switch off is local", cfg: conf(false), want: false},
		{name: "switch on is remote", cfg: conf(true), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.TemplateBuildRemote(); got != tc.want {
				t.Fatalf("TemplateBuildRemote() = %t, want %t", got, tc.want)
			}
		})
	}
}

// TemplateCenterRequired drives endpoint validation: it must only fire when a
// template path actually depends on TC. With a single boolean switch that is
// simply "the switch is on".
func TestTemplateCenterRequiredFollowsSwitch(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{name: "nil config is not required", cfg: nil, want: false},
		// Switch off: TC is not in the request path, so an empty endpoint is
		// fine (this is the pure "installed but local" one-click default).
		{name: "switch off is not required", cfg: conf(false), want: false},
		{name: "switch on is required", cfg: conf(true), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.TemplateCenterRequired(); got != tc.want {
				t.Fatalf("TemplateCenterRequired() = %t, want %t", got, tc.want)
			}
		})
	}
}

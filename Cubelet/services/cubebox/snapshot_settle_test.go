// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseStatJiffies(t *testing.T) {
	// Realistic /proc stat line: comm contains a space and parens.
	line := "12345 (vcpu0) S 1 12345 12345 0 -1 4194304 100 0 0 0 1600 240 0 0 20 0 1 0 50 1000000 100 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 2 0 0 0 0 0"
	utime, stime, err := parseStatJiffies(line)
	if err != nil {
		t.Fatalf("parseStatJiffies failed: %v", err)
	}
	if utime != 1600 || stime != 240 {
		t.Fatalf("unexpected jiffies: utime=%d stime=%d", utime, stime)
	}
}

func TestParseStatJiffiesMalformed(t *testing.T) {
	if _, _, err := parseStatJiffies("no-paren-line"); err == nil {
		t.Fatal("expected error for malformed line")
	}
	if _, _, err := parseStatJiffies("1 (x) S 1 2"); err == nil {
		t.Fatal("expected error for short line")
	}
}

func TestLoadSettleConfigDefaults(t *testing.T) {
	for _, k := range []string{
		"CUBE_TEMPLATE_SETTLE_ENABLED",
		"CUBE_TEMPLATE_SETTLE_POLL_INTERVAL_MS",
		"CUBE_TEMPLATE_SETTLE_QUIET_WINDOW_MS",
		"CUBE_TEMPLATE_SETTLE_MIN_WAIT_MS",
		"CUBE_TEMPLATE_SETTLE_MAX_WAIT_MS",
		"CUBE_TEMPLATE_SETTLE_BUSY_THRESHOLD",
	} {
		t.Setenv(k, "")
	}
	cfg := loadSettleConfig()
	if !cfg.Enabled {
		t.Fatal("gate should default to enabled")
	}
	if cfg.QuietWindow < cfg.PollInterval {
		t.Fatal("quiet window must cover at least one poll")
	}
	if cfg.MaxWait <= 0 || cfg.MinWait < 0 {
		t.Fatal("invalid wait bounds")
	}
	if cfg.BusyThreshold <= 0 || cfg.BusyThreshold >= 1 {
		t.Fatal("invalid busy threshold")
	}
}

func TestLoadSettleConfigEnvOverride(t *testing.T) {
	t.Setenv("CUBE_TEMPLATE_SETTLE_ENABLED", "false")
	t.Setenv("CUBE_TEMPLATE_SETTLE_MAX_WAIT_MS", "30000")
	t.Setenv("CUBE_TEMPLATE_SETTLE_BUSY_THRESHOLD", "0.05")
	cfg := loadSettleConfig()
	if cfg.Enabled {
		t.Fatal("env override of Enabled failed")
	}
	if cfg.MaxWait != 30*time.Second {
		t.Fatalf("MaxWait=%v, want 30s", cfg.MaxWait)
	}
	if cfg.BusyThreshold != 0.05 {
		t.Fatalf("BusyThreshold=%v, want 0.05", cfg.BusyThreshold)
	}
}

type fakeSettleLogger struct{ infos, warns int }

func (l *fakeSettleLogger) Infof(format string, args ...interface{}) { l.infos++ }
func (l *fakeSettleLogger) Warnf(format string, args ...interface{}) { l.warns++ }

func TestWaitGuestSettledDisabled(t *testing.T) {
	t.Setenv("CUBE_TEMPLATE_SETTLE_ENABLED", "false")
	l := &fakeSettleLogger{}
	res := waitGuestSettled(context.Background(), "nonexistent-sandbox", l)
	if res.Settled || res.TimedOut {
		t.Fatal("disabled gate must neither settle nor time out")
	}
	if !strings.Contains(res.Skipped, "disabled") {
		t.Fatalf("unexpected skip reason: %q", res.Skipped)
	}
	if l.warns == 0 {
		t.Fatal("disabled gate should log a warning for auditability")
	}
}

func TestWaitGuestSettledNoShim(t *testing.T) {
	l := &fakeSettleLogger{}
	start := time.Now()
	res := waitGuestSettled(context.Background(), "definitely-no-such-sandbox-id", l)
	if res.Skipped == "" {
		t.Fatal("expected skip when shim is absent")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("missing shim must not block the build for long")
	}
}

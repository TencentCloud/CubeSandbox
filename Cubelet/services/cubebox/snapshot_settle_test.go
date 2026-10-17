// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
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

func TestIsVcpuThreadName(t *testing.T) {
	for _, ok := range []string{"vcpu0", "vcpu1", "vcpu123"} {
		if !isVcpuThreadName(ok) {
			t.Fatalf("%q should be recognized as a vCPU thread", ok)
		}
	}
	for _, bad := range []string{"vcpu", "vcpu-worker", "vhost0", "tokio-runtime-w", ""} {
		if isVcpuThreadName(bad) {
			t.Fatalf("%q must not be recognized as a vCPU thread", bad)
		}
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
		"CUBE_TEMPLATE_SETTLE_QUIET_RATIO",
		"CUBE_TEMPLATE_SETTLE_BAIL_AFTER_MS",
	} {
		t.Setenv(k, "")
	}
	cfg := loadSettleConfig()
	if want := runtime.GOARCH == "arm64"; cfg.Enabled != want {
		t.Fatalf("default Enabled=%v, want %v on %s", cfg.Enabled, want, runtime.GOARCH)
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
	if cfg.QuietRatio <= 0 || cfg.QuietRatio > 1 {
		t.Fatal("invalid quiet ratio")
	}
	if cfg.BailAfter <= 0 {
		t.Fatal("bail-after should default to a positive duration")
	}
}

func TestLoadSettleConfigEnvOverride(t *testing.T) {
	t.Setenv("CUBE_TEMPLATE_SETTLE_ENABLED", "false")
	t.Setenv("CUBE_TEMPLATE_SETTLE_MAX_WAIT_MS", "30000")
	t.Setenv("CUBE_TEMPLATE_SETTLE_BUSY_THRESHOLD", "0.05")
	t.Setenv("CUBE_TEMPLATE_SETTLE_QUIET_RATIO", "0.5")
	t.Setenv("CUBE_TEMPLATE_SETTLE_BAIL_AFTER_MS", "20000")
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
	if cfg.QuietRatio != 0.5 {
		t.Fatalf("QuietRatio=%v, want 0.5", cfg.QuietRatio)
	}
	if cfg.BailAfter != 20*time.Second {
		t.Fatalf("BailAfter=%v, want 20s", cfg.BailAfter)
	}
}

func TestLoadSettleConfigEnvOptIn(t *testing.T) {
	t.Setenv("CUBE_TEMPLATE_SETTLE_ENABLED", "true")
	cfg := loadSettleConfig()
	if !cfg.Enabled {
		t.Fatal("env opt-in must enable the gate on any architecture")
	}
}

func TestApplyYamlSettleConfig(t *testing.T) {
	base := settleConfig{
		Enabled:       false,
		PollInterval:  200 * time.Millisecond,
		QuietWindow:   2 * time.Second,
		MinWait:       time.Second,
		MaxWait:       60 * time.Second,
		BusyThreshold: 0.1,
		QuietRatio:    0.8,
		BailAfter:     10 * time.Second,
	}
	enabled := true
	bail := time.Duration(0)
	applyYamlSettleConfig(&base, config.TemplateSettleConf{
		Enabled:    &enabled,
		MaxWait:    30 * time.Second,
		QuietRatio: 0.5,
		BailAfter:  &bail, // explicit 0s must disable the early bail
	})
	if !base.Enabled {
		t.Fatal("yaml enabled=true not applied")
	}
	if base.MaxWait != 30*time.Second {
		t.Fatalf("MaxWait=%v, want 30s", base.MaxWait)
	}
	if base.QuietRatio != 0.5 {
		t.Fatalf("QuietRatio=%v, want 0.5", base.QuietRatio)
	}
	if base.BailAfter != 0 {
		t.Fatalf("BailAfter=%v, want 0 (disabled)", base.BailAfter)
	}
	// Unset fields must leave the built-in defaults untouched.
	if base.PollInterval != 200*time.Millisecond || base.MinWait != time.Second ||
		base.QuietWindow != 2*time.Second || base.BusyThreshold != 0.1 {
		t.Fatalf("unset yaml fields clobbered defaults: %+v", base)
	}
}

// TestLoadSettleConfigPrecedence exercises the full chain against a real
// config file: built-in defaults < YAML (common.template_settle) < env.
func TestLoadSettleConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf.yaml")
	yaml := "common:\n  template_settle:\n    enabled: true\n    max_wait: 30s\n    quiet_ratio: 0.5\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Init(path, false); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	t.Cleanup(func() { config.Init("", true) })

	cfg := loadSettleConfig()
	if !cfg.Enabled {
		t.Fatal("yaml enabled=true must win over the built-in arch default")
	}
	if cfg.MaxWait != 30*time.Second || cfg.QuietRatio != 0.5 {
		t.Fatalf("yaml values not applied: %+v", cfg)
	}

	// env stays the highest-precedence override.
	t.Setenv("CUBE_TEMPLATE_SETTLE_MAX_WAIT_MS", "45000")
	t.Setenv("CUBE_TEMPLATE_SETTLE_ENABLED", "false")
	cfg = loadSettleConfig()
	if cfg.MaxWait != 45*time.Second {
		t.Fatalf("env MaxWait=%v, want 45s (env must override yaml)", cfg.MaxWait)
	}
	if cfg.Enabled {
		t.Fatal("env ENABLED=false must override yaml enabled=true")
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

// fakeSettleClock advances deterministically by d on every After call, so
// loop tests run instantly with production-realistic timing parameters.
type fakeSettleClock struct{ now time.Time }

func (c *fakeSettleClock) Now() time.Time { return c.now }
func (c *fakeSettleClock) After(d time.Duration) <-chan time.Time {
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

// testLoopConfig mirrors the production defaults; with a 200ms fake poll,
// n jiffies of delta per poll is busy = n/20, so delta<=2 is quiet and
// delta=10 is busy under the 0.1 threshold.
func testLoopConfig() settleConfig {
	return settleConfig{
		Enabled:       true,
		PollInterval:  200 * time.Millisecond,
		QuietWindow:   2 * time.Second,
		MinWait:       time.Second,
		MaxWait:       60 * time.Second,
		BusyThreshold: 0.1,
		QuietRatio:    0.8,
		BailAfter:     10 * time.Second,
	}
}

// scriptedSampler returns base on the first (baseline) call, then adds
// deltas[0], deltas[1], ... on each poll; once the script is exhausted the
// value holds steady (zero delta, i.e. quiet).
func scriptedSampler(base uint64, deltas ...uint64) vcpuSampler {
	cur := base
	calls := 0
	return func(int) (uint64, error) {
		if calls > 0 && calls-1 < len(deltas) {
			cur += deltas[calls-1]
		}
		calls++
		return cur, nil
	}
}

func runFakeLoop(cfg settleConfig, sample vcpuSampler) settleResult {
	t0 := time.Unix(1700000000, 0)
	clock := &fakeSettleClock{now: t0}
	res := settleResult{}
	runSettleLoop(context.Background(), cfg, 1234, sample, clock, t0, &res)
	return res
}

// periodicBusySampler adds a busy delta (10 jiffies) on every poll whose
// index is a multiple of period, simulating a periodic guest task.
func periodicBusySampler(period int) vcpuSampler {
	cur := uint64(0)
	calls := 0
	return func(int) (uint64, error) {
		if calls > 0 && calls%period == 0 {
			cur += 10
		}
		calls++
		return cur, nil
	}
}

// alwaysBusySampler adds a busy delta on every poll (CPU-bound steady state).
func alwaysBusySampler() vcpuSampler {
	cur := uint64(0)
	return func(int) (uint64, error) { cur += 10; return cur, nil }
}

func TestSettleLoopSettlesAfterQuietWindow(t *testing.T) {
	res := runFakeLoop(testLoopConfig(), scriptedSampler(1000))
	// The sliding window (2s = 10 polls) fills with quiet samples at poll 10
	// (t=2.0s), ratio 1.0 >= 0.8, past MinWait=1s.
	if !res.Settled {
		t.Fatalf("expected settle, got %+v", res)
	}
	if res.Samples != 10 {
		t.Fatalf("Samples=%d, want 10 (one full 2s window)", res.Samples)
	}
}

func TestSettleLoopRespectsMinWait(t *testing.T) {
	cfg := testLoopConfig()
	cfg.MinWait = 5 * time.Second
	res := runFakeLoop(cfg, scriptedSampler(1000))
	if !res.Settled {
		t.Fatalf("expected settle, got %+v", res)
	}
	if res.Samples != 25 {
		t.Fatalf("Samples=%d, want 25 (MinWait=5s at 200ms polls)", res.Samples)
	}
}

func TestSettleLoopBusySpikeTolerated(t *testing.T) {
	// One busy spike inside the window drops the ratio to 0.9, still >= 0.8:
	// the gate passes on schedule instead of restarting the window.
	deltas := make([]uint64, 5)
	deltas[4] = 10 // busy at poll 5
	res := runFakeLoop(testLoopConfig(), scriptedSampler(1000, deltas...))
	if !res.Settled {
		t.Fatalf("expected settle, got %+v", res)
	}
	if res.Samples != 10 {
		t.Fatalf("Samples=%d, want 10 (single spike must not delay the pass)", res.Samples)
	}
}

func TestSettleLoopPeriodicNoisePasses(t *testing.T) {
	// A periodic task every 1s (every 5th poll) is 2 busy samples per
	// 10-sample window: ratio exactly 0.8 passes. Under the old
	// uninterrupted-quiet rule such a guest could NEVER settle and every
	// build of its image would pay the full MaxWait.
	res := runFakeLoop(testLoopConfig(), periodicBusySampler(5))
	if !res.Settled {
		t.Fatalf("expected settle, got %+v", res)
	}
	if res.Samples != 10 {
		t.Fatalf("Samples=%d, want 10", res.Samples)
	}
}

func TestSettleLoopFrequentNoiseTimesOut(t *testing.T) {
	// Busy every 2nd poll pins the ratio at 0.5 < 0.8; quiet samples exist,
	// so early bail must NOT fire and the gate waits the full MaxWait.
	res := runFakeLoop(testLoopConfig(), periodicBusySampler(2))
	if !res.TimedOut || res.Settled || res.Bailed {
		t.Fatalf("expected pure timeout, got %+v", res)
	}
	if res.Samples != 300 {
		t.Fatalf("Samples=%d, want 300 (60s MaxWait at 200ms polls)", res.Samples)
	}
}

func TestSettleLoopThresholdBoundaryIsQuiet(t *testing.T) {
	// 2 jiffies per 200ms poll is exactly the 0.1 threshold; the inclusive
	// boundary must treat it as quiet, not spin to timeout.
	alwaysTwo := func() vcpuSampler {
		cur := uint64(0)
		return func(int) (uint64, error) { cur += 2; return cur, nil }
	}
	res := runFakeLoop(testLoopConfig(), alwaysTwo())
	if !res.Settled {
		t.Fatalf("boundary busy=threshold must be quiet, got %+v", res)
	}
}

func TestSettleLoopNoQuietBailsEarly(t *testing.T) {
	// Never a single quiet sample (CPU-bound steady state): the gate bails
	// at BailAfter (10s = poll 50) instead of burning the full 60s MaxWait.
	res := runFakeLoop(testLoopConfig(), alwaysBusySampler())
	if !res.Bailed || res.Settled || res.TimedOut {
		t.Fatalf("expected early bail, got %+v", res)
	}
	if res.Samples != 50 {
		t.Fatalf("Samples=%d, want 50 (10s BailAfter at 200ms polls)", res.Samples)
	}
}

func TestSettleLoopBailDisabledTimesOut(t *testing.T) {
	cfg := testLoopConfig()
	cfg.BailAfter = 0 // explicit opt-out
	res := runFakeLoop(cfg, alwaysBusySampler())
	if !res.TimedOut || res.Settled || res.Bailed {
		t.Fatalf("expected pure timeout, got %+v", res)
	}
	if res.Samples != 300 {
		t.Fatalf("Samples=%d, want 300", res.Samples)
	}
}

func TestSettleLoopRareQuietPreventsBail(t *testing.T) {
	// One quiet sample every 10 polls means the guest is not in CPU-bound
	// steady state: no early bail, but ratio 0.1 never passes -> timeout.
	cur := uint64(0)
	calls := 0
	mostlyBusy := func(int) (uint64, error) {
		if calls > 0 && calls%10 != 0 {
			cur += 10
		}
		calls++
		return cur, nil
	}
	res := runFakeLoop(testLoopConfig(), mostlyBusy)
	if !res.TimedOut || res.Bailed || res.Settled {
		t.Fatalf("expected pure timeout, got %+v", res)
	}
	if res.Samples != 300 {
		t.Fatalf("Samples=%d, want 300", res.Samples)
	}
}

func TestSettleLoopInitialSampleError(t *testing.T) {
	fail := func(int) (uint64, error) { return 0, fmt.Errorf("proc gone") }
	res := runFakeLoop(testLoopConfig(), fail)
	if !strings.Contains(res.Skipped, "read vcpu stat failed") {
		t.Fatalf("unexpected skip reason: %q", res.Skipped)
	}
	if res.Samples != 0 {
		t.Fatalf("Samples=%d, want 0", res.Samples)
	}
}

func TestSettleLoopMidWaitSampleError(t *testing.T) {
	calls := 0
	flakey := func(int) (uint64, error) {
		calls++
		if calls >= 3 {
			return 0, fmt.Errorf("proc gone")
		}
		return 1000, nil
	}
	res := runFakeLoop(testLoopConfig(), flakey)
	if !strings.Contains(res.Skipped, "mid-wait") {
		t.Fatalf("unexpected skip reason: %q", res.Skipped)
	}
	if res.Samples != 1 {
		t.Fatalf("Samples=%d, want 1 (one good poll before the failure)", res.Samples)
	}
}

func TestSettleLoopContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t0 := time.Unix(1700000000, 0)
	clock := &fakeSettleClock{now: t0}
	res := settleResult{}
	runSettleLoop(ctx, testLoopConfig(), 1234, scriptedSampler(1000), clock, t0, &res)
	if !strings.Contains(res.Skipped, "context done") {
		t.Fatalf("unexpected skip reason: %q", res.Skipped)
	}
	if res.Samples != 0 {
		t.Fatalf("Samples=%d, want 0", res.Samples)
	}
}

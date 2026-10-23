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
	"strconv"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
)

// snapshot_settle.go implements the "settle gate" for template creation.
//
// Background: AppSnapshot freezes a freshly created cubebox with a FULL
// memory snapshot almost immediately after Create returns. At that moment
// the guest may still be settling (boot daemons, timers, virtio/vsock queue
// state, GIC state). Everything not yet settled is frozen into the template
// and re-paid on EVERY later restore as transport-path latency (vcpu halt ->
// wakeup, interrupt delivery, host scheduling), which is the root cause of
// the template-to-template performance variance.
//
// The gate waits until the guest's vCPUs are observed mostly idle over a
// sliding window (a quiet RATIO, so periodic noise cannot reset progress)
// before allowing the snapshot to proceed. It samples vCPU thread CPU time
// from /proc on the host, which adds ZERO load to the guest (any agent-RPC
// based probing would itself inject vsock traffic into the very channel we
// want to go quiet).
//
// The gate is strictly best-effort: any error (process not found, timeout,
// ctx cancelled) is logged and snapshot proceeds. It must never fail or
// block a template build indefinitely.
//
// Deadline budget: the gate runs on the caller's context, which carries the
// master's AppSnapshotTimeoutInSec (default 300s) deadline, so MaxWait (60s)
// can consume up to ~20% of the total build budget. On cold pipelines with
// large image pulls, lower CUBE_TEMPLATE_SETTLE_MAX_WAIT_MS if builds ever
// run tight against that deadline.

const (
	// procUserHz is USER_HZ on Linux: jiffies per second reported by
	// /proc/[pid]/task/[tid]/stat.
	procUserHz = 100.0

	// shimExePrefix matches the containerd shim binary that hosts the
	// in-process VMM (in 0.5.1 cube_hypervisor::VmmInstance runs as threads
	// of the shim process, see CubeShim hypervisor/cube_hypervisor.rs and
	// common/utils.rs record_pid which writes the shim pid as VMM_PID_FILE).
	shimExePrefix = "containerd-shim-cube"

	// vcpuThreadPrefix prefixes vCPU thread names created by the hypervisor
	// (hypervisor/vmm/src/cpu.rs: .name(format!("vcpu{}", vcpu_id))).
	// Matching requires a numeric suffix so future vcpu-* helper threads
	// are not mistaken for guest CPUs.
	vcpuThreadPrefix = "vcpu"
)

// settleLogger is satisfied by *cubelog.Entry (log.G(ctx).WithFields(...)).
// Declared as a narrow local interface so this file needs no cubelog import.
type settleLogger interface {
	Infof(format string, args ...interface{})
	Warnf(format string, args ...interface{})
}

// settleConfig tunes the settle gate. All fields are overridable via env so
// operators can adapt per machine without rebuilding.
type settleConfig struct {
	Enabled       bool
	PollInterval  time.Duration
	QuietWindow   time.Duration // sliding evaluation window for the quiet ratio
	MinWait       time.Duration
	MaxWait       time.Duration
	BusyThreshold float64 // vCPU busy ratio, in units of one CPU, summed over vcpus
	// QuietRatio is the minimum fraction of quiet samples inside the sliding
	// QuietWindow for the guest to count as settled. A ratio (not
	// uninterrupted quiet) is used so periodic noise -- systemd timers,
	// kworker workqueues, agent heartbeats -- cannot reset progress every
	// cycle and pin every build of such an image at the full MaxWait.
	QuietRatio float64
	// BailAfter bounds the wait for CPU-bound images where busy is the
	// steady state: if not a single quiet sample was seen after BailAfter,
	// further waiting cannot help and the gate bails early instead of
	// burning the full MaxWait on every build of such an image. 0 disables.
	BailAfter time.Duration
}

// The WFE-exit settle problem this gate addresses has only been observed on
// ARM64 (Kunpeng), so Enabled defaults to true only there; other
// architectures default to off and keep the pre-gate behavior unless an
// operator explicitly opts in via CUBE_TEMPLATE_SETTLE_ENABLED=true.
//
// Precedence: built-in defaults < cubelet YAML config (common.template_settle,
// hot-reloaded) < CUBE_TEMPLATE_SETTLE_* env vars.
func loadSettleConfig() settleConfig {
	cfg := settleConfig{
		Enabled:       runtime.GOARCH == "arm64",
		PollInterval:  200 * time.Millisecond,
		QuietWindow:   2 * time.Second,
		MinWait:       1 * time.Second,
		MaxWait:       60 * time.Second,
		BusyThreshold: 0.1,
		QuietRatio:    0.8,
		BailAfter:     10 * time.Second,
	}
	applyYamlSettleConfig(&cfg, config.GetTemplateSettle())
	cfg.Enabled = settleEnvBool("CUBE_TEMPLATE_SETTLE_ENABLED", cfg.Enabled)
	cfg.PollInterval = settleEnvDurationMs("CUBE_TEMPLATE_SETTLE_POLL_INTERVAL_MS", cfg.PollInterval)
	cfg.QuietWindow = settleEnvDurationMs("CUBE_TEMPLATE_SETTLE_QUIET_WINDOW_MS", cfg.QuietWindow)
	cfg.MinWait = settleEnvDurationMs("CUBE_TEMPLATE_SETTLE_MIN_WAIT_MS", cfg.MinWait)
	cfg.MaxWait = settleEnvDurationMs("CUBE_TEMPLATE_SETTLE_MAX_WAIT_MS", cfg.MaxWait)
	cfg.BusyThreshold = settleEnvFloat("CUBE_TEMPLATE_SETTLE_BUSY_THRESHOLD", cfg.BusyThreshold)
	cfg.QuietRatio = settleEnvFloat("CUBE_TEMPLATE_SETTLE_QUIET_RATIO", cfg.QuietRatio)
	cfg.BailAfter = settleEnvDurationMs("CUBE_TEMPLATE_SETTLE_BAIL_AFTER_MS", cfg.BailAfter)
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 200 * time.Millisecond
	}
	if cfg.QuietWindow < cfg.PollInterval {
		cfg.QuietWindow = cfg.PollInterval
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 60 * time.Second
	}
	if cfg.BusyThreshold <= 0 {
		cfg.BusyThreshold = 0.1
	}
	if cfg.QuietRatio <= 0 || cfg.QuietRatio > 1 {
		cfg.QuietRatio = 0.8
	}
	if cfg.BailAfter < 0 {
		cfg.BailAfter = 10 * time.Second
	}
	return cfg
}

// applyYamlSettleConfig layers the cubelet managed YAML config
// (common.template_settle) over the built-in defaults. Zero/nil fields mean
// "not configured" and leave the default untouched; the env vars are applied
// after this and stay the highest-precedence override.
func applyYamlSettleConfig(cfg *settleConfig, yc config.TemplateSettleConf) {
	if yc.Enabled != nil {
		cfg.Enabled = *yc.Enabled
	}
	if yc.MaxWait > 0 {
		cfg.MaxWait = yc.MaxWait
	}
	if yc.PollInterval > 0 {
		cfg.PollInterval = yc.PollInterval
	}
	if yc.QuietWindow > 0 {
		cfg.QuietWindow = yc.QuietWindow
	}
	if yc.MinWait > 0 {
		cfg.MinWait = yc.MinWait
	}
	if yc.BusyThreshold > 0 {
		cfg.BusyThreshold = yc.BusyThreshold
	}
	if yc.QuietRatio > 0 {
		cfg.QuietRatio = yc.QuietRatio
	}
	if yc.BailAfter != nil {
		cfg.BailAfter = *yc.BailAfter // an explicit 0s disables the early bail
	}
}

// The settleEnv* helpers are name-scoped to this file's feature to avoid
// colliding with generic env helpers elsewhere in package cubebox.
func settleEnvBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func settleEnvDurationMs(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return time.Duration(n) * time.Millisecond
}

func settleEnvFloat(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// settleResult is the audit record of one gate run; logged on every build.
type settleResult struct {
	Settled    bool
	TimedOut   bool
	Bailed     bool   // gave up early: no quiet sample within BailAfter
	Skipped    string // non-empty when the gate did not run / did not wait
	Waited     time.Duration
	Samples    int
	LastBusy   float64
	QuietRatio float64 // quiet fraction of the last full sliding window
}

// vcpuSampler returns cumulative vCPU jiffies (utime+stime) for a pid.
// readVcpuJiffies in production, scripted in tests.
type vcpuSampler func(pid int) (uint64, error)

// settleClock abstracts time so the settle loop can be unit-tested
// deterministically. realSettleClock is used in production.
type settleClock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realSettleClock struct{}

func (realSettleClock) Now() time.Time                         { return time.Now() }
func (realSettleClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// waitGuestSettled blocks until the guest vCPUs of sandboxID stay mostly
// quiet over a sliding window (quiet ratio >= the configured threshold), or
// until MaxWait elapses or the gate bails early on a permanently busy guest.
// Best-effort: never returns an error; always safe to ignore the result.
func waitGuestSettled(ctx context.Context, sandboxID string, logger settleLogger) settleResult {
	start := time.Now()
	res := settleResult{}
	cfg := loadSettleConfig()

	finish := func() settleResult {
		res.Waited = time.Since(start)
		switch {
		case res.Skipped != "":
			logger.Warnf("settle-gate: skipped (%s), template is frozen without settle check", res.Skipped)
		case res.Settled:
			logger.Infof("settle-gate: guest settled after %v (%d samples, quiet ratio=%.2f, last busy=%.4f)",
				res.Waited, res.Samples, res.QuietRatio, res.LastBusy)
		case res.Bailed:
			logger.Warnf("settle-gate: no quiet sample in %v (%d samples, last busy=%.4f); "+
				"bailing early, snapshot proceeds anyway, this template may carry unsettled guest state",
				res.Waited, res.Samples, res.LastBusy)
		case res.TimedOut:
			logger.Warnf("settle-gate: timeout after %v (%d samples, quiet ratio=%.2f, last busy=%.4f); "+
				"snapshot proceeds anyway, this template may carry unsettled guest state",
				res.Waited, res.Samples, res.QuietRatio, res.LastBusy)
		}
		return res
	}

	if !cfg.Enabled {
		res.Skipped = "gate disabled (default on only for arm64; enable via common.template_settle.enabled or CUBE_TEMPLATE_SETTLE_ENABLED=true)"
		return finish()
	}

	pid, err := findShimPIDWithRetry(ctx, sandboxID, 5, 200*time.Millisecond)
	if err != nil {
		res.Skipped = fmt.Sprintf("shim process not found: %v", err)
		return finish()
	}

	runSettleLoop(ctx, cfg, pid, readVcpuJiffies, realSettleClock{}, start, &res)
	return finish()
}

// runSettleLoop is the testable core of the gate: poll the sampler until the
// quiet ratio over the sliding window reaches the configured threshold, or
// until MaxWait elapses, BailAfter gives up, or ctx is done. The outcome is
// recorded into res; start is the gate entry time (deadline and MinWait are
// measured from it).
func runSettleLoop(ctx context.Context, cfg settleConfig, pid int, sample vcpuSampler, clock settleClock, start time.Time, res *settleResult) {
	deadline := start.Add(cfg.MaxWait)
	prevJiffies, err := sample(pid)
	if err != nil {
		res.Skipped = fmt.Sprintf("read vcpu stat failed: %v", err)
		return
	}
	prevAt := clock.Now()

	winSize := int(cfg.QuietWindow / cfg.PollInterval)
	if winSize < 1 {
		winSize = 1
	}
	window := make([]bool, 0, winSize) // recent sample quiet flags, oldest first
	quietInWindow := 0
	totalQuiet := 0

	for {
		select {
		case <-ctx.Done():
			res.Skipped = fmt.Sprintf("context done: %v", ctx.Err())
			return
		default:
		}

		now := clock.Now()
		if !now.Before(deadline) {
			res.TimedOut = true
			return
		}

		select {
		case <-ctx.Done():
			res.Skipped = fmt.Sprintf("context done: %v", ctx.Err())
			return
		case <-clock.After(cfg.PollInterval):
		}

		curJiffies, err := sample(pid)
		if err != nil {
			res.Skipped = fmt.Sprintf("read vcpu stat failed mid-wait: %v", err)
			return
		}
		now = clock.Now()

		wallSec := now.Sub(prevAt).Seconds()
		if wallSec <= 0 {
			continue
		}
		busy := float64(curJiffies-prevJiffies) / (wallSec * procUserHz)
		prevJiffies, prevAt = curJiffies, now
		res.Samples++
		res.LastBusy = busy

		// Inclusive boundary: at the default threshold of 0.1, exactly 2
		// jiffies in one 200ms poll is exactly 0.1 and must count as quiet,
		// otherwise guests sitting exactly at the boundary never settle.
		quiet := busy <= cfg.BusyThreshold
		if len(window) == winSize {
			if window[0] {
				quietInWindow--
			}
			window = window[1:]
		}
		window = append(window, quiet)
		if quiet {
			quietInWindow++
			totalQuiet++
		}
		res.QuietRatio = float64(quietInWindow) / float64(len(window))

		elapsed := now.Sub(start)

		// For CPU-bound images busy is the steady state: if not a single
		// quiet sample showed up after BailAfter, waiting longer cannot
		// help. Bail early instead of burning the full MaxWait on every
		// build of such an image. Fail-safe: the build proceeds with
		// pre-gate template quality.
		if cfg.BailAfter > 0 && totalQuiet == 0 && elapsed >= cfg.BailAfter {
			res.Bailed = true
			return
		}

		// Pass on a quiet RATIO over the sliding window, not on
		// uninterrupted quiet: a single busy sample must not reset
		// progress, or any guest with a periodic task shorter than the
		// window would never pass.
		if elapsed >= cfg.MinWait && len(window) == winSize && res.QuietRatio >= cfg.QuietRatio {
			res.Settled = true
			return
		}
	}
}

// findShimPIDWithRetry locates the containerd shim process of the sandbox.
// The shim (containerd-shim-cube-rs) hosts the VMM in-process, so its vcpu*
// threads ARE the guest's CPUs.
func findShimPIDWithRetry(ctx context.Context, sandboxID string, tries int, interval time.Duration) (int, error) {
	var lastErr error
	for i := 0; i < tries; i++ {
		pid, err := findShimPID(sandboxID)
		if err == nil {
			return pid, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(interval):
		}
	}
	return 0, lastErr
}

// findShimPID scans /proc for the shim process: exe basename has the
// containerd-shim-cube prefix AND the sandbox id appears as a cmdline arg
// (containerd shim v2 convention: `containerd-shim-cube-rs -namespace <ns>
// -id <sandboxID> -address <addr>`).
func findShimPID(sandboxID string) (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, fmt.Errorf("read /proc: %w", err)
	}
	found := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		exe, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if err != nil {
			continue // process may have exited
		}
		if !strings.HasPrefix(filepath.Base(exe), shimExePrefix) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		for _, arg := range strings.Split(string(raw), "\x00") {
			if arg == sandboxID {
				if found != 0 && found != pid {
					return 0, fmt.Errorf("multiple shim processes match sandbox %s: %d and %d", sandboxID, found, pid)
				}
				found = pid
			}
		}
	}
	if found == 0 {
		return 0, fmt.Errorf("no shim process found for sandbox %s", sandboxID)
	}
	return found, nil
}

// isVcpuThreadName reports whether comm is a hypervisor vCPU thread name:
// "vcpu" followed by a numeric vCPU id (hypervisor/vmm/src/cpu.rs). The
// numeric-suffix requirement keeps future vcpu-* helper threads from being
// mistaken for guest CPUs.
func isVcpuThreadName(comm string) bool {
	if !strings.HasPrefix(comm, vcpuThreadPrefix) {
		return false
	}
	suffix := comm[len(vcpuThreadPrefix):]
	if suffix == "" {
		return false
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// readVcpuJiffies sums utime+stime (jiffies) over all vcpu* threads of the
// shim process. Threads of the shim that are not vCPUs (vhost, control
// loop, tokio workers) are intentionally excluded: they do host-side work
// that may continue after the guest is idle.
func readVcpuJiffies(pid int) (uint64, error) {
	taskDir := filepath.Join("/proc", strconv.Itoa(pid), "task")
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", taskDir, err)
	}
	var total uint64
	var vcpus int
	for _, e := range entries {
		commRaw, err := os.ReadFile(filepath.Join(taskDir, e.Name(), "comm"))
		if err != nil {
			continue
		}
		if !isVcpuThreadName(strings.TrimSpace(string(commRaw))) {
			continue
		}
		statRaw, err := os.ReadFile(filepath.Join(taskDir, e.Name(), "stat"))
		if err != nil {
			continue
		}
		utime, stime, err := parseStatJiffies(string(statRaw))
		if err != nil {
			continue
		}
		total += utime + stime
		vcpus++
	}
	if vcpus == 0 {
		return 0, fmt.Errorf("no vcpu* threads found in pid %d", pid)
	}
	return total, nil
}

// parseStatJiffies extracts utime (field 14) and stime (field 15) from a
// /proc stat line. comm may contain spaces/parens, so parse after the LAST
// ')'. After comm, fields are: state(3) ppid(4) pgrp(5) session(6)
// tty_nr(7) tpgid(8) flags(9) minflt(10) cminflt(11) majflt(12) cmajflt(13)
// utime(14) stime(15) -> rest[11] and rest[12].
func parseStatJiffies(stat string) (uint64, uint64, error) {
	idx := strings.LastIndex(stat, ")")
	if idx < 0 || idx+2 > len(stat) {
		return 0, 0, fmt.Errorf("malformed stat line")
	}
	rest := strings.Fields(stat[idx+1:])
	if len(rest) < 13 {
		return 0, 0, fmt.Errorf("short stat line: %d fields after comm", len(rest))
	}
	utime, err := strconv.ParseUint(rest[11], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse utime: %w", err)
	}
	stime, err := strconv.ParseUint(rest[12], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse stime: %w", err)
	}
	return utime, stime, nil
}

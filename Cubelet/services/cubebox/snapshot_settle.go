// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
// The gate waits until the guest's vCPUs are observed idle for a sustained
// window before allowing the snapshot to proceed. It samples vCPU thread
// CPU time from /proc on the host, which adds ZERO load to the guest (any
// agent-RPC based probing would itself inject vsock traffic into the very
// channel we want to go quiet).
//
// The gate is strictly best-effort: any error (process not found, timeout,
// ctx cancelled) is logged and snapshot proceeds. It must never fail or
// block a template build indefinitely.

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
	QuietWindow   time.Duration
	MinWait       time.Duration
	MaxWait       time.Duration
	BusyThreshold float64 // vCPU busy ratio, in units of one CPU, summed over vcpus
}

func loadSettleConfig() settleConfig {
	cfg := settleConfig{
		Enabled:       true,
		PollInterval:  200 * time.Millisecond,
		QuietWindow:   2 * time.Second,
		MinWait:       1 * time.Second,
		MaxWait:       60 * time.Second,
		BusyThreshold: 0.1,
	}
	cfg.Enabled = envBool("CUBE_TEMPLATE_SETTLE_ENABLED", cfg.Enabled)
	cfg.PollInterval = envDurationMs("CUBE_TEMPLATE_SETTLE_POLL_INTERVAL_MS", cfg.PollInterval)
	cfg.QuietWindow = envDurationMs("CUBE_TEMPLATE_SETTLE_QUIET_WINDOW_MS", cfg.QuietWindow)
	cfg.MinWait = envDurationMs("CUBE_TEMPLATE_SETTLE_MIN_WAIT_MS", cfg.MinWait)
	cfg.MaxWait = envDurationMs("CUBE_TEMPLATE_SETTLE_MAX_WAIT_MS", cfg.MaxWait)
	cfg.BusyThreshold = envFloat("CUBE_TEMPLATE_SETTLE_BUSY_THRESHOLD", cfg.BusyThreshold)
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
	return cfg
}

func envBool(key string, def bool) bool {
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

func envDurationMs(key string, def time.Duration) time.Duration {
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

func envFloat(key string, def float64) float64 {
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
	Settled  bool
	TimedOut bool
	Skipped  string // non-empty when the gate did not run / did not wait
	Waited   time.Duration
	Samples  int
	LastBusy float64
}

// waitGuestSettled blocks until the guest vCPUs of sandboxID stay below the
// busy threshold for a full quiet window, or until MaxWait elapses.
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
			logger.Infof("settle-gate: guest settled after %v (%d samples, last busy=%.4f)",
				res.Waited, res.Samples, res.LastBusy)
		case res.TimedOut:
			logger.Warnf("settle-gate: timeout after %v (%d samples, last busy=%.4f); "+
				"snapshot proceeds anyway, this template may carry unsettled guest state",
				res.Waited, res.Samples, res.LastBusy)
		}
		return res
	}

	if !cfg.Enabled {
		res.Skipped = "disabled by CUBE_TEMPLATE_SETTLE_ENABLED"
		return finish()
	}

	pid, err := findShimPIDWithRetry(ctx, sandboxID, 5, 200*time.Millisecond)
	if err != nil {
		res.Skipped = fmt.Sprintf("shim process not found: %v", err)
		return finish()
	}

	deadline := start.Add(cfg.MaxWait)
	prevJiffies, err := readVcpuJiffies(pid)
	if err != nil {
		res.Skipped = fmt.Sprintf("read vcpu stat failed: %v", err)
		return finish()
	}
	prevAt := time.Now()

	var quietSince time.Time
	for {
		select {
		case <-ctx.Done():
			res.Skipped = fmt.Sprintf("context done: %v", ctx.Err())
			return finish()
		default:
		}

		now := time.Now()
		if !now.Before(deadline) {
			res.TimedOut = true
			return finish()
		}

		select {
		case <-ctx.Done():
			res.Skipped = fmt.Sprintf("context done: %v", ctx.Err())
			return finish()
		case <-time.After(cfg.PollInterval):
		}

		curJiffies, err := readVcpuJiffies(pid)
		if err != nil {
			res.Skipped = fmt.Sprintf("read vcpu stat failed mid-wait: %v", err)
			return finish()
		}
		now = time.Now()

		wallSec := now.Sub(prevAt).Seconds()
		if wallSec <= 0 {
			continue
		}
		busy := float64(curJiffies-prevJiffies) / (wallSec * procUserHz)
		prevJiffies, prevAt = curJiffies, now
		res.Samples++
		res.LastBusy = busy

		if busy < cfg.BusyThreshold {
			if quietSince.IsZero() {
				quietSince = now
			}
		} else {
			quietSince = time.Time{}
		}

		elapsed := now.Sub(start)
		if elapsed >= cfg.MinWait && !quietSince.IsZero() && now.Sub(quietSince) >= cfg.QuietWindow {
			res.Settled = true
			return finish()
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
		if !strings.HasPrefix(strings.TrimSpace(string(commRaw)), vcpuThreadPrefix) {
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

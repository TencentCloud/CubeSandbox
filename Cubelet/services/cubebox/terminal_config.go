// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/services/cubebox/terminalcore"
)

type TerminalServicesConfig struct {
	ReconnectGraceSeconds    int `toml:"reconnect_grace_seconds"`
	ReplayBufferBytes        int `toml:"replay_buffer_bytes"`
	MaxFrameBytes            int `toml:"max_frame_bytes"`
	StdinQueueFrames         int `toml:"stdin_queue_frames"`
	StdoutChunkBytes         int `toml:"stdout_chunk_bytes"`
	StdoutPendingBytes       int `toml:"stdout_pending_bytes"`
	MaxSessions              int `toml:"max_sessions"`
	MaxSessionsPerSandbox    int `toml:"max_sessions_per_sandbox"`
	MaxSessionsPerContainer  int `toml:"max_sessions_per_container"`
	ResizeCoalesceMillis     int `toml:"resize_coalesce_millis"`
	OpenTimeoutSeconds       int `toml:"open_timeout_seconds"`
	IdleTimeoutMinutes       int `toml:"idle_timeout_minutes"`
	MaxLifetimeHours         int `toml:"max_lifetime_hours"`
	CleanupGraceSeconds      int `toml:"cleanup_grace_seconds"`
	CleanupTimeoutSeconds    int `toml:"cleanup_timeout_seconds"`
	DrainTimeoutSeconds      int `toml:"drain_timeout_seconds"`
	ReconcileIntervalSeconds int `toml:"reconcile_interval_seconds"`
}

func defaultTerminalServicesConfig() TerminalServicesConfig {
	defaults := terminalcore.DefaultConfig()
	return TerminalServicesConfig{
		ReconnectGraceSeconds:    int(defaults.ReconnectGrace / time.Second),
		ReplayBufferBytes:        defaults.ReplayBufferBytes,
		MaxFrameBytes:            defaults.MaxFrameBytes,
		StdinQueueFrames:         defaults.StdinQueueFrames,
		StdoutChunkBytes:         defaults.StdoutChunkBytes,
		StdoutPendingBytes:       defaults.StdoutPendingBytes,
		MaxSessions:              defaults.MaxSessions,
		MaxSessionsPerSandbox:    defaults.MaxSessionsSandbox,
		MaxSessionsPerContainer:  defaults.MaxSessionsContainer,
		ResizeCoalesceMillis:     int(defaults.ResizeCoalesce / time.Millisecond),
		OpenTimeoutSeconds:       int(defaults.OpenTimeout / time.Second),
		IdleTimeoutMinutes:       int(defaults.IdleTimeout / time.Minute),
		MaxLifetimeHours:         int(defaults.MaxLifetime / time.Hour),
		CleanupGraceSeconds:      int(defaults.CleanupGrace / time.Second),
		CleanupTimeoutSeconds:    int(defaults.CleanupTimeout / time.Second),
		DrainTimeoutSeconds:      15,
		ReconcileIntervalSeconds: int(defaults.ReconcileInterval / time.Second),
	}
}

func (c TerminalServicesConfig) coreConfig() (terminalcore.Config, time.Duration, error) {
	config := terminalcore.Config{
		MaxFrameBytes:        c.MaxFrameBytes,
		StdinQueueFrames:     c.StdinQueueFrames,
		StdoutChunkBytes:     c.StdoutChunkBytes,
		StdoutPendingBytes:   c.StdoutPendingBytes,
		ReplayBufferBytes:    c.ReplayBufferBytes,
		MaxSessions:          c.MaxSessions,
		MaxSessionsSandbox:   c.MaxSessionsPerSandbox,
		MaxSessionsContainer: c.MaxSessionsPerContainer,
		ResizeCoalesce:       time.Duration(c.ResizeCoalesceMillis) * time.Millisecond,
		OpenTimeout:          time.Duration(c.OpenTimeoutSeconds) * time.Second,
		ReconnectGrace:       time.Duration(c.ReconnectGraceSeconds) * time.Second,
		IdleTimeout:          time.Duration(c.IdleTimeoutMinutes) * time.Minute,
		MaxLifetime:          time.Duration(c.MaxLifetimeHours) * time.Hour,
		CleanupGrace:         time.Duration(c.CleanupGraceSeconds) * time.Second,
		CleanupTimeout:       time.Duration(c.CleanupTimeoutSeconds) * time.Second,
		ReconcileInterval:    time.Duration(c.ReconcileIntervalSeconds) * time.Second,
	}
	if err := config.Validate(); err != nil {
		return terminalcore.Config{}, 0, err
	}
	drainTimeout := time.Duration(c.DrainTimeoutSeconds) * time.Second
	if drainTimeout <= 0 {
		return terminalcore.Config{}, 0, fmt.Errorf("terminal drain timeout must be positive")
	}
	return config, drainTimeout, nil
}

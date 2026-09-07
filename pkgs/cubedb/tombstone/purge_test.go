// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package tombstone

import (
	"context"
	"testing"
	"time"
)

func TestConfig_sanitized_DefaultsAndClamps(t *testing.T) {
	cases := []struct {
		name string
		in   Config
		want Config
	}{
		{
			name: "zero retention clamps to default",
			in:   Config{Retention: 0},
			want: Config{Retention: defaultRetention, Interval: defaultInterval, BatchSize: defaultBatchSize, MaxPerPass: defaultMaxPerPass},
		},
		{
			name: "negative retention clamps to default (foot-gun: cutoff>=now)",
			in:   Config{Retention: -5 * time.Minute},
			want: Config{Retention: defaultRetention, Interval: defaultInterval, BatchSize: defaultBatchSize, MaxPerPass: defaultMaxPerPass},
		},
		{
			name: "sub-min retention clamps to min",
			in:   Config{Retention: 30 * time.Minute},
			want: Config{Retention: minRetention, Interval: defaultInterval, BatchSize: defaultBatchSize, MaxPerPass: defaultMaxPerPass},
		},
		{
			name: "valid retention preserved",
			in:   Config{Retention: 48 * time.Hour},
			want: Config{Retention: 48 * time.Hour, Interval: defaultInterval, BatchSize: defaultBatchSize, MaxPerPass: defaultMaxPerPass},
		},
		{
			name: "zero interval clamps to default",
			in:   Config{Interval: 0},
			want: Config{Retention: defaultRetention, Interval: defaultInterval, BatchSize: defaultBatchSize, MaxPerPass: defaultMaxPerPass},
		},
		{
			name: "sub-min interval clamps to min",
			in:   Config{Interval: 5 * time.Second},
			want: Config{Retention: defaultRetention, Interval: minInterval, BatchSize: defaultBatchSize, MaxPerPass: defaultMaxPerPass},
		},
		{
			name: "batch above max_per_pass is lowered to the cap",
			in:   Config{BatchSize: 500, MaxPerPass: 10},
			want: Config{Retention: defaultRetention, Interval: defaultInterval, BatchSize: 10, MaxPerPass: 10},
		},
		{
			name: "explicit sane values preserved",
			in:   Config{Retention: 24 * time.Hour, Interval: 30 * time.Minute, BatchSize: 100, MaxPerPass: 1000},
			want: Config{Retention: 24 * time.Hour, Interval: 30 * time.Minute, BatchSize: 100, MaxPerPass: 1000},
		},
		{
			name: "oversized batch clamped to max",
			in:   Config{BatchSize: 999999},
			want: Config{Retention: defaultRetention, Interval: defaultInterval, BatchSize: maxBatchSize, MaxPerPass: defaultMaxPerPass},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.sanitized()
			if got.Retention != tc.want.Retention {
				t.Errorf("Retention = %v, want %v", got.Retention, tc.want.Retention)
			}
			if got.Interval != tc.want.Interval {
				t.Errorf("Interval = %v, want %v", got.Interval, tc.want.Interval)
			}
			if got.BatchSize != tc.want.BatchSize {
				t.Errorf("BatchSize = %v, want %v", got.BatchSize, tc.want.BatchSize)
			}
			if got.MaxPerPass != tc.want.MaxPerPass {
				t.Errorf("MaxPerPass = %v, want %v", got.MaxPerPass, tc.want.MaxPerPass)
			}
		})
	}
}

// sanitized must be idempotent so re-application (e.g. a config-reload path)
// never drifts.
func TestConfig_sanitized_Idempotent(t *testing.T) {
	c := Config{Retention: -1, Interval: time.Second, BatchSize: 0, MaxPerPass: 1}.sanitized()
	again := c.sanitized()
	// Config contains a slice (Tables) and a func (OnPurge), so it is not
	// comparable with ==; compare the fields sanitized() touches.
	if again.Retention != c.Retention || again.Interval != c.Interval ||
		again.BatchSize != c.BatchSize || again.MaxPerPass != c.MaxPerPass {
		t.Errorf("sanitized not idempotent:\n first  = %+v\n second = %+v", c, again)
	}
}

// Start is a no-op on misconfiguration and must not panic.
func TestStart_NoOpOnInvalidConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// nil db, empty lock, empty tables, disabled — none should launch a goroutine.
	Start(ctx, nil, Config{LockName: "x", Tables: []string{"t"}})
	Start(ctx, nil, Config{Enabled: true}) // db nil
	// A valid-shaped but disabled config must also be a no-op.
	Start(ctx, nil, Config{Enabled: false, LockName: "x", Tables: []string{"t"}})
}

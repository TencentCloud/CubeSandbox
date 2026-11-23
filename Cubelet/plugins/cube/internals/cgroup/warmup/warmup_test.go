// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warmup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPercpuUsageBytes(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name    string
		content string
		want    uint64
		wantOK  bool
	}{
		{
			name:    "reports the per-cpu allocator footprint",
			content: "MemTotal:       790570520 kB\nPercpu:          6898176 kB\nMemFree:   777335528 kB\n",
			want:    6898176 * 1024,
			wantOK:  true,
		},
		{
			name:    "kernel without the field",
			content: "MemTotal:       790570520 kB\nMemFree:   777335528 kB\n",
			wantOK:  false,
		},
		{
			name:    "unparsable value is treated as unknown",
			content: "Percpu:        unknown kB\n",
			wantOK:  false,
		},
		{
			// "RemoteMemTotal" used to be matched by an unanchored grep when this
			// was a shell helper, and one such node reported 0 for everything.
			name:    "similar prefix is not matched",
			content: "Node 0 PercpuRemote: 1 kB\nPercpu:  2048 kB\n",
			want:    2048 * 1024,
			wantOK:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "meminfo")
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o644))

			got, ok, err := PercpuUsageBytes(path)
			require.NoError(t, err)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestPercpuUsageBytesMissingFile(t *testing.T) {
	_, ok, err := PercpuUsageBytes(filepath.Join(t.TempDir(), "nope"))
	assert.False(t, ok)
	assert.Error(t, err)
}

func TestResolve(t *testing.T) {
	const mib = 1024 * 1024

	tests := []struct {
		name       string
		limits     Limits
		cost       uint64
		costKnown  bool
		want       int
		wantReason string
	}{
		{
			// Hand-built pools in tests leave the bounds at their zero value and
			// must keep the historical behaviour.
			name:       "no bounds keeps the requested size",
			limits:     Limits{Requested: 3000},
			cost:       65 * mib,
			costKnown:  true,
			want:       3000,
			wantReason: "no bound configured",
		},
		{
			name:       "hard cap alone",
			limits:     Limits{Requested: 3000, Max: 256, Min: 2},
			costKnown:  false,
			want:       256,
			wantReason: "warm_pool_max",
		},
		{
			// The headline regression: a many-core host where one cgroup costs
			// ~65 MiB of kernel percpu must not warm thousands of slots.
			name:       "budget wins over a huge request on a many-core host",
			limits:     Limits{Requested: 3000, Max: 256, Min: 2, BudgetMB: 256},
			cost:       65 * mib,
			costKnown:  true,
			want:       3, // 256 MiB / 65 MiB
			wantReason: "warm_pool_mem_budget_mb",
		},
		{
			name:       "cheap slots keep the capped request",
			limits:     Limits{Requested: 3000, Max: 256, Min: 2, BudgetMB: 256},
			cost:       512 * 1024, // 0.5 MiB
			costKnown:  true,
			want:       256,
			wantReason: "warm_pool_max",
		},
		{
			name:       "unknown cost falls back to the configured bounds",
			limits:     Limits{Requested: 3000, Max: 256, Min: 2, BudgetMB: 256},
			costKnown:  false,
			want:       256,
			wantReason: "warm_pool_max",
		},
		{
			name:       "a single slot over budget still warms one",
			limits:     Limits{Requested: 3000, Max: 256, Min: 2, BudgetMB: 16},
			cost:       65 * mib,
			costKnown:  true,
			want:       2, // budget allows 0 -> 1, then the floor lifts it to 2
			wantReason: "warm_pool_min",
		},
		{
			name:       "zero cost does not divide by zero",
			limits:     Limits{Requested: 3000, Max: 256, Min: 2, BudgetMB: 256},
			cost:       0,
			costKnown:  true,
			want:       256,
			wantReason: "warm_pool_max",
		},
		{
			name:       "request below the cap is left alone",
			limits:     Limits{Requested: 8, Max: 256, Min: 2, BudgetMB: 256},
			cost:       512 * 1024,
			costKnown:  true,
			want:       8,
			wantReason: "within bounds",
		},
		{
			name:       "negative bounds disable them",
			limits:     Limits{Requested: 3000, Max: -1, Min: -1, BudgetMB: -1},
			cost:       65 * mib,
			costKnown:  true,
			want:       3000,
			wantReason: "no bound configured",
		},
		{
			name:       "zero request still warms one slot",
			limits:     Limits{Requested: 0, Max: 256, Min: 2, BudgetMB: 256},
			cost:       512 * 1024,
			costKnown:  true,
			want:       2,
			wantReason: "warm_pool_min",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := Resolve(tc.limits, tc.cost, tc.costKnown)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantReason, reason)
		})
	}
}

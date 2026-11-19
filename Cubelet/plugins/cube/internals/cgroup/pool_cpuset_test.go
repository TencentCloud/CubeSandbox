// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/allocator"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/numa"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

// applyCpusetFiles is what repairs a per-NUMA slot on every start, so cover the
// three states it can find: empty, drifted and already correct.
func TestApplyCpusetFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"cpuset.cpus", "cpuset.mems"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0644))
	}

	require.NoError(t, applyCpusetFiles(dir, "16-31,208-223", 1))
	require.Equal(t, "16-31,208-223", readCpusetFile(t, dir, "cpuset.cpus"))
	require.Equal(t, "1", readCpusetFile(t, dir, "cpuset.mems"))

	// A CPU hot-add or a manual edit leaves a stale mask behind.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cpuset.cpus"), []byte("0-3"), 0644))
	require.NoError(t, applyCpusetFiles(dir, "16-31,208-223", 1))
	require.Equal(t, "16-31,208-223", readCpusetFile(t, dir, "cpuset.cpus"))

	// A second run against a healthy slot is a no-op, not an error.
	require.NoError(t, applyCpusetFiles(dir, "16-31,208-223", 1))

	// A missing directory is an error: the caller must learn that the mask was
	// never applied instead of starting with an unbound pool.
	require.Error(t, applyCpusetFiles(filepath.Join(dir, "missing"), "0", 0))
}

func readCpusetFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)
	return string(b)
}

// cpusetTestHandle drives the pool bootstrap without a real cgroupfs.
type cpusetTestHandle struct {
	handle.Interface

	createErr error
	dir       string
	dirErr    error
}

func (h *cpusetTestHandle) IsExist(context.Context, string) bool { return true }
func (h *cpusetTestHandle) Create(context.Context, string) error { return nil }

func (h *cpusetTestHandle) CreateWithCpuSet(context.Context, string, string, int) error {
	return h.createErr
}

func (h *cpusetTestHandle) CpusetDir(string) (string, error) {
	if h.dirErr != nil {
		return "", h.dirErr
	}
	return h.dir, nil
}

func newCpusetTestPool(t *testing.T, h handle.Interface, strict bool) *cgPool {
	t.Helper()
	ranger, err := allocator.NewSimpleLinearRanger(0, 1)
	require.NoError(t, err)
	return &cgPool{
		initialSize:  1,
		poolV1Handle: h,
		poolV2Handle: h,
		poolV2: cgPoolV2{
			handle: h,
			numaPools: []cgPoolV2Numa{{
				allocator: allocator.NewAllocator[uint16](ranger),
				cgRanger:  ranger,
				numaNode:  numa.NumaNode{NodeId: 0, Cpulist: "0-3"},
			}},
		},
		reparentFileEnabled: func() bool { return false },
		numaAffinityInUse:   func() bool { return strict },
	}
}

func newSlotDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"cpuset.cpus", "cpuset.mems"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0644))
	}
	return dir
}

// strict decides whether a broken slot stops startup, which is the whole point
// of the switch: with it off the pool is unused and startup stays tolerant, with
// it on a slot that carries no mask must not be handed out.
func TestInitPoolV2LateStrictness(t *testing.T) {
	t.Run("strict, apply fails, slot missing", func(t *testing.T) {
		h := &cpusetTestHandle{dir: filepath.Join(t.TempDir(), "missing")}
		err := newCpusetTestPool(t, h, true).initPoolV2Late()
		require.Error(t, err)
		require.Contains(t, err.Error(), "cpuset")
	})

	t.Run("strict, apply fails, create fails too", func(t *testing.T) {
		h := &cpusetTestHandle{
			createErr: errors.New("enable cpuset controller"),
			dirErr:    errors.New("cpuset controller is not available"),
		}
		err := newCpusetTestPool(t, h, true).initPoolV2Late()
		require.Error(t, err)
		// Both halves have to survive: the create error is what explains why.
		require.Contains(t, err.Error(), "enable cpuset controller")
		require.Contains(t, err.Error(), "cpuset controller is not available")
	})

	t.Run("strict, create fails but slot was repaired", func(t *testing.T) {
		// The slot of a previous run: create reports it exists, the re-assert
		// above proves the mask is in place, so startup continues.
		h := &cpusetTestHandle{createErr: errors.New("slot already exists"), dir: newSlotDir(t)}
		require.NoError(t, newCpusetTestPool(t, h, true).initPoolV2Late())
	})

	t.Run("strict, healthy slot", func(t *testing.T) {
		h := &cpusetTestHandle{dir: newSlotDir(t)}
		require.NoError(t, newCpusetTestPool(t, h, true).initPoolV2Late())
	})

	t.Run("not strict, apply fails", func(t *testing.T) {
		// Switch off: the pool is unused, so a slot that cannot be repaired is
		// only a warning, exactly as before the switch existed.
		h := &cpusetTestHandle{dirErr: errors.New("cpuset controller is not available")}
		require.NoError(t, newCpusetTestPool(t, h, false).initPoolV2Late())
	})
}

// A single-NUMA host gains nothing from the per-NUMA pool, and that fallback is
// what keeps existing non-NUMA deployments on the v1 pool.
func TestNumaAffinityEnabledSingleNumaHost(t *testing.T) {
	old := numaNodeCount
	t.Cleanup(func() { numaNodeCount = old })

	numaNodeCount = func() int { return 1 }
	require.False(t, (&Config{EnableNumaAffinity: true}).NumaAffinityEnabled())

	numaNodeCount = func() int { return 12 }
	require.True(t, (&Config{EnableNumaAffinity: true}).NumaAffinityEnabled())
	require.False(t, (&Config{EnableNumaAffinity: false}).NumaAffinityEnabled())

	var nilConfig *Config
	require.False(t, nilConfig.NumaAffinityEnabled())
}

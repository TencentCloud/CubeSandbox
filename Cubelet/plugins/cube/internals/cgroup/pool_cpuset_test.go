// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/allocator"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/numa"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
	v2 "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle/v2"
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

// A write that the kernel does not keep would leave the slot unbound while the
// bootstrap reports success, so the value is read back and compared.
func TestApplyCpusetFilesRejectsSwallowedWrite(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cpuset.mems"), nil, 0644))
	// /dev/null swallows the write, so the read-back cannot find the mask.
	require.NoError(t, os.Symlink(os.DevNull, filepath.Join(dir, "cpuset.cpus")))

	err := applyCpusetFiles(dir, "0-3", 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "reads back")
}

// cgroupfs prints the effective set rather than the text it was given, and
// removeDisabledCpus can hand it a list that is not canonical, so the read-back
// has to compare sets: a text comparison rejects a slot that is bound.
func TestSameCPUSet(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", "0-15,192-207", "0-15,192-207", true},
		{"single-element range prints as one id", "0", "0-0", true},
		{"overlapping ranges print merged", "0-95", "0-6,6-95", true},
		{"adjacent ranges print merged", "0-95", "0-47,48-95", true},
		{"adjacent single ids print merged", "0-95", "0-6,7-95", true},
		{"one id short", "0-95", "0-94", false},
		{"untouched against untouched", "", " ", true},
		{"untouched against a mask", "", "0-3", false},
		{"unparsable list", "0-3", "x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, sameCPUSet(tc.a, tc.b))
		})
	}
}

// A slot that already names the right set is left alone even when the text
// differs, so a host whose cpulist is non-canonical keeps a working slot
// instead of failing startup.
func TestApplyCpusetFilesKeepsKernelSpelling(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cpuset.cpus"), []byte("0-95\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cpuset.mems"), []byte("0"), 0644))

	// What removeDisabledCpus emits for node cpulist 0-95 with an unsorted
	// cgroup_disable_cpuset_list of "7,5"; the kernel prints it back as "0-95".
	require.NoError(t, applyCpusetFiles(dir, "0-6,6-95", 0))
	require.Equal(t, "0-95\n", readCpusetFile(t, dir, "cpuset.cpus"))
}

// The unified hierarchy has no per-controller mount, so the mask lives in the
// cgroup's own files and CpusetDir hands back that directory. The v1 backend
// resolves it from /proc/mounts instead and has no injection point here.
func TestV2CpusetDirPointsAtTheCgroupItself(t *testing.T) {
	h := v2.NewV2Handle(2)
	group := "cube_sandbox_v2/sandbox/numa3"
	dir, err := h.CpusetDir(group)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(handle.RootMountPoint, group), dir)
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
	// dirs maps a specific group to its directory, so the node directory and
	// the sandbox slots below it can be told apart.
	dirs   map[string]string
	dirErr error
}

func (h *cpusetTestHandle) IsExist(context.Context, string) bool { return true }
func (h *cpusetTestHandle) Create(context.Context, string) error { return nil }

func (h *cpusetTestHandle) CreateWithCpuSet(context.Context, string, string, int) error {
	return h.createErr
}

func (h *cpusetTestHandle) CpusetDir(group string) (string, error) {
	if h.dirErr != nil {
		return "", h.dirErr
	}
	if dir, ok := h.dirs[group]; ok {
		return dir, nil
	}
	return h.dir, nil
}

func newCpusetTestPool(t *testing.T, h handle.Interface, strict bool) *cgPool {
	t.Helper()
	return newCpusetTestPoolWithCPUs(t, h, strict, "0-3")
}

// newCpusetTestPoolWithCPUs also sets the node's cpulist, so the empty-mask
// case (a memory-only node) can be driven without a fake numa sysfs.
func newCpusetTestPoolWithCPUs(t *testing.T, h handle.Interface, strict bool, cpulist string) *cgPool {
	t.Helper()
	ranger, err := allocator.NewSimpleLinearRanger(0, 1)
	require.NoError(t, err)
	prevReparent := setupMemoryReparentFileFn
	setupMemoryReparentFileFn = func(context.Context, uint32, bool) error { return nil }
	t.Cleanup(func() { setupMemoryReparentFileFn = prevReparent })
	return &cgPool{
		initialSize:  1,
		poolV1Handle: h,
		poolV2Handle: h,
		poolV2: cgPoolV2{
			handle: h,
			numaPools: []cgPoolV2Numa{{
				allocator: allocator.NewAllocator[uint16](ranger),
				cgRanger:  ranger,
				numaNode:  numa.NumaNode{NodeId: 0, Cpulist: cpulist},
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

// A v1 slot copies the parent mask when it is created, so one that predates a
// CPU hot-add keeps the old mask; with the switch on the bootstrap repairs it,
// otherwise a sandbox placed on it silently misses the new CPUs.
func TestInitPoolV2LateRepairsSandboxSlots(t *testing.T) {
	nodeDir := newSlotDir(t)
	slotDir := newSlotDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(slotDir, "cpuset.cpus"), []byte("0"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(slotDir, "cpuset.mems"), []byte("0"), 0644))

	group := MakeCgroupPathByID(MakeFullCgID(0, true, 0))
	h := &cpusetTestHandle{dir: nodeDir, dirs: map[string]string{group: slotDir}}
	require.NoError(t, newCpusetTestPool(t, h, true).initPoolV2Late())

	require.Equal(t, "0-3", readCpusetFile(t, slotDir, "cpuset.cpus"))
	require.Equal(t, "0", readCpusetFile(t, slotDir, "cpuset.mems"))
}

// With the switch off the pool is not handed out, so the bootstrap has to leave
// the slots exactly as it found them.
func TestInitPoolV2LateLeavesSandboxSlotsAloneWhenOff(t *testing.T) {
	nodeDir := newSlotDir(t)
	slotDir := newSlotDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(slotDir, "cpuset.cpus"), []byte("0"), 0644))

	group := MakeCgroupPathByID(MakeFullCgID(0, true, 0))
	h := &cpusetTestHandle{dir: nodeDir, dirs: map[string]string{group: slotDir}}
	require.NoError(t, newCpusetTestPool(t, h, false).initPoolV2Late())

	require.Equal(t, "0", readCpusetFile(t, slotDir, "cpuset.cpus"))
}

// A slot without cpuset files (nothing to repair there, e.g. an unset child on
// the unified hierarchy) must not make the bootstrap complain.
func TestInitPoolV2LateToleratesSlotWithoutCpuset(t *testing.T) {
	nodeDir := newSlotDir(t)
	group := MakeCgroupPathByID(MakeFullCgID(0, true, 0))
	h := &cpusetTestHandle{
		dir:  nodeDir,
		dirs: map[string]string{group: filepath.Join(t.TempDir(), "gone")},
	}
	require.NoError(t, newCpusetTestPool(t, h, true).initPoolV2Late())
}

// strict decides whether a broken slot stops startup, which is the whole point
// of the switch: with it on a slot that carries no mask must not be handed out.
// With it off the pool is unused and the path stays the one from before the
// switch, so a create failure is still fatal there and only the re-assert is a
// warning.
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

	// A memory-only node reports an empty cpulist; writing that is accepted by
	// the kernel but leaves the slot without any mask, which is exactly what the
	// strict path exists to reject.
	t.Run("strict, node has no cpus", func(t *testing.T) {
		h := &cpusetTestHandle{dir: newSlotDir(t)}
		err := newCpusetTestPoolWithCPUs(t, h, true, "").initPoolV2Late()
		require.Error(t, err)
		require.Contains(t, err.Error(), "no cpus to bind")
	})

	t.Run("strict, healthy slot", func(t *testing.T) {
		h := &cpusetTestHandle{dir: newSlotDir(t)}
		require.NoError(t, newCpusetTestPool(t, h, true).initPoolV2Late())
	})

	t.Run("not strict, apply fails", func(t *testing.T) {
		// Switch off: the pool is unused, and the re-assert is new here, so it is
		// only a warning rather than a reason not to start.
		h := &cpusetTestHandle{dirErr: errors.New("cpuset controller is not available")}
		require.NoError(t, newCpusetTestPool(t, h, false).initPoolV2Late())
	})

	t.Run("not strict, create fails", func(t *testing.T) {
		// Before the switch existed this returned the error, so it still does:
		// a default deployment must not start on a host where the v2 pool root
		// cannot be created.
		h := &cpusetTestHandle{createErr: errors.New("create pool v2 root")}
		err := newCpusetTestPool(t, h, false).initPoolV2Late()
		require.Error(t, err)
		require.Contains(t, err.Error(), "create pool v2 root")
	})
}

// A single-NUMA host gains nothing from the per-NUMA pool, and that fallback is
// what keeps existing non-NUMA deployments on the v1 pool.
func TestNumaAffinityEnabled(t *testing.T) {
	// The pool reads this var without a lock, so these subtests must not be
	// parallelised.
	old := numaNodeCount
	t.Cleanup(func() { numaNodeCount = old })

	var nilConfig *Config
	for _, tc := range []struct {
		name  string
		cfg   *Config
		nodes int
		want  bool
	}{
		{"nil config", nilConfig, 12, false},
		{"switch off", &Config{EnableNumaAffinity: false}, 12, false},
		{"switch on, single NUMA", &Config{EnableNumaAffinity: true}, 1, false},
		{"switch on, multi NUMA", &Config{EnableNumaAffinity: true}, 12, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			numaNodeCount = func() int { return tc.nodes }
			require.Equal(t, tc.want, tc.cfg.NumaAffinityEnabled())
		})
	}
}

// numanodes come from os.ReadDir, which sorts lexicographically: on a host with
// 11+ nodes the slice order is node0, node1, node10, node11, node2, ... The pool
// has to be indexed by node id, otherwise a node's slots are range-checked with
// another node's ranger and tidyPoolV2 deletes slots that are in use.
func TestInitPoolV2IndexesByNodeID(t *testing.T) {
	old := getAllNumaNodes
	t.Cleanup(func() { getAllNumaNodes = old })

	order := []int{0, 1, 10, 11, 2, 3, 4, 5, 6, 7, 8, 9}
	getAllNumaNodes = func() []numa.NumaNode {
		nodes := make([]numa.NumaNode, 0, len(order))
		for _, id := range order {
			nodes = append(nodes, numa.NumaNode{NodeId: id, Cpulist: fmt.Sprintf("%d", id)})
		}
		return nodes
	}

	p := &cgPool{initialSize: 2}
	require.NoError(t, p.initPoolV2())
	require.Len(t, p.poolV2.numaPools, 12)
	for i := range p.poolV2.numaPools {
		require.Equal(t, i, p.poolV2.numaPools[i].numaNode.NodeId, "numaPools[%d]", i)
	}
}

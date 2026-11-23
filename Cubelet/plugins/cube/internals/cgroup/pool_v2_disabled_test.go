// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/numa"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

const poolTestTriggerInterval = 10 * time.Millisecond

// poolCalls records what a poolTestHandle was asked to do, so a test can assert
// that an upgrade with the switch off creates nothing and deletes nothing.
type poolCalls struct {
	created   []string
	cpus      []string
	deleted   []string
	cleaned   []string
	subdirs   []string
	usage     []string
	quotaRead []string
}

// poolTestHandle is a handle.Interface that records the calls the pool makes.
type poolTestHandle struct {
	handle.Interface

	mutex sync.Mutex
	// exist overrides IsExist for a group; existDefault answers the rest.
	exist        map[string]bool
	existDefault bool
	// cpusetDir is what CpusetDir resolves to; empty means the controller is
	// unavailable, which the bootstrap rejects.
	cpusetDir string

	calls poolCalls

	cpuQuota   int
	memQuota   int64
	usageValue handle.UsageSnapshot
}

func newPoolTestHandle(t *testing.T) *poolTestHandle {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"cpuset.cpus", "cpuset.mems"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0644))
	}
	return &poolTestHandle{exist: map[string]bool{}, cpusetDir: dir}
}

func (h *poolTestHandle) IsExist(_ context.Context, group string) bool {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	if v, ok := h.exist[group]; ok {
		return v
	}
	return h.existDefault
}

func (h *poolTestHandle) setExist(group string, exist bool) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.exist[group] = exist
}

func (h *poolTestHandle) Create(_ context.Context, group string) error {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.calls.created = append(h.calls.created, group)
	return nil
}

func (h *poolTestHandle) CreateWithCpuSet(_ context.Context, group, _ string, _ int) error {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.calls.cpus = append(h.calls.cpus, group)
	return nil
}

func (h *poolTestHandle) Delete(_ context.Context, group string) error {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.calls.deleted = append(h.calls.deleted, group)
	return nil
}

func (h *poolTestHandle) CleanForReuse(_ context.Context, group string) error {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.calls.cleaned = append(h.calls.cleaned, group)
	return nil
}

func (h *poolTestHandle) ListSubdir(subdir string) ([]string, error) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.calls.subdirs = append(h.calls.subdirs, subdir)
	return nil, nil
}

func (h *poolTestHandle) List() ([]string, error) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	out := make([]string, 0, len(h.calls.created))
	for _, group := range h.calls.created {
		out = append(out, filepath.Base(group))
	}
	return out, nil
}

func (h *poolTestHandle) UsageSnapshot(_ context.Context, group string) (handle.UsageSnapshot, error) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.calls.usage = append(h.calls.usage, group)
	return h.usageValue, nil
}

func (h *poolTestHandle) GetAllocatedCpuNum(group string) int {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.calls.quotaRead = append(h.calls.quotaRead, group)
	return h.cpuQuota
}

func (h *poolTestHandle) GetAllocatedMem(group string) int64 {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.calls.quotaRead = append(h.calls.quotaRead, group)
	return h.memQuota
}

func (h *poolTestHandle) CpusetDir(group string) (string, error) {
	if h.cpusetDir == "" {
		return "", fmt.Errorf("cpuset directory of %s is not available", group)
	}
	return h.cpusetDir, nil
}

// reset drops the recorded calls so each restart phase of a multi-phase test can
// be asserted on its own.
func (h *poolTestHandle) reset() {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.calls = poolCalls{}
}

// recorded returns a copy of the calls made so far.
func (h *poolTestHandle) recorded() poolCalls {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return poolCalls{
		created:   append([]string(nil), h.calls.created...),
		cpus:      append([]string(nil), h.calls.cpus...),
		deleted:   append([]string(nil), h.calls.deleted...),
		cleaned:   append([]string(nil), h.calls.cleaned...),
		subdirs:   append([]string(nil), h.calls.subdirs...),
		usage:     append([]string(nil), h.calls.usage...),
		quotaRead: append([]string(nil), h.calls.quotaRead...),
	}
}

// stubNumaNodes pins the NUMA topology for one test; the pool reads it through a
// package variable so no NUMA host is needed.
func stubNumaNodes(t *testing.T, nodes ...numa.NumaNode) {
	t.Helper()
	prev := getAllNumaNodes
	getAllNumaNodes = func() []numa.NumaNode { return nodes }
	t.Cleanup(func() { getAllNumaNodes = prev })
}

// newPoolForTest builds a pool the way the plugin does: two handles, a temporary
// bolt store so DB recovery is exercised, and the switch injected.
func newPoolForTest(t *testing.T, v1, v2 handle.Interface, size int, poolV2Enabled bool) (*cgPool, *utils.CubeStore) {
	t.Helper()
	db, err := utils.NewCubeStoreExt(filepath.Join(t.TempDir(), "db"), "meta.db", 4, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return newPoolForTestWithStore(t, v1, v2, size, poolV2Enabled, db), db
}

// newPoolForTestWithStore reuses an existing store, which models a cubelet restart.
func newPoolForTestWithStore(t *testing.T, v1, v2 handle.Interface, size int, poolV2Enabled bool, db *utils.CubeStore) *cgPool {
	t.Helper()
	prevReparent := setupMemoryReparentFileFn
	setupMemoryReparentFileFn = func(context.Context, uint32, bool) error { return nil }
	t.Cleanup(func() { setupMemoryReparentFileFn = prevReparent })

	return &cgPool{
		initialSize:         size,
		poolV1Handle:        v1,
		poolV2Handle:        v2,
		db:                  db,
		workers:             1,
		triggerInterval:     poolTestTriggerInterval,
		reparentFileEnabled: func() bool { return false },
		numaAffinityInUse:   func() bool { return poolV2Enabled },
	}
}

func recordInUse(t *testing.T, db *utils.CubeStore, sandboxID string, fullCgID uint32) {
	t.Helper()
	require.NoError(t, db.Set(bucket, sandboxID, []byte(strconv.Itoa(int(fullCgID)))))
}

// The whole point of the change: with the switch off nothing of the per-NUMA
// pool is created, listed or swept.
func TestInitSkipsPoolV2WhenDisabled(t *testing.T) {
	v1, v2 := newPoolTestHandle(t), newPoolTestHandle(t)
	p, db := newPoolForTest(t, v1, v2, 4, false)
	recordInUse(t, db, "sb-v1", 2)

	require.NoError(t, p.init())
	t.Cleanup(p.stop)

	got := v2.recorded()
	require.Empty(t, got.cpus, "no per-NUMA node directory may be created")
	require.Empty(t, got.subdirs, "no per-NUMA directory may be listed")
	require.Empty(t, got.deleted)
	require.False(t, p.poolV2.enabled, "the per-NUMA pool must not be bootstrapped")
	require.Empty(t, p.poolV2.numaPools)

	// pool v1 is untouched: its slots are still pre-created and the DB entry
	// recovered. The ranger is inclusive of initialSize, so 4 yields 5 slots.
	require.Len(t, v1.recorded().created, 5)
	require.Equal(t, toInts([]uint32{2}), toInts(p.All()), "only the recovered slot is assigned")

	// Nothing may ask for a v2 slot.
	_, err := p.Get(context.Background(), "sb-new", true, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not enabled")

	// Tidy has no NUMA directory to walk and reports no error.
	require.Empty(t, p.Tidy())
	require.Empty(t, v2.recorded().subdirs, "Tidy must not list the per-NUMA pool")

	groups, err := p.GetAllGroupsExists()
	require.NoError(t, err)
	// The fake handle reports the pre-created pool v1 directories, numbered 0-4.
	require.ElementsMatch(t, []uint32{0, 1, 2, 3, 4}, groups)
}

// With the switch on the pool is bootstrapped exactly as before.
func TestInitPreProvisionsPoolV2WhenEnabled(t *testing.T) {
	v1, v2 := newPoolTestHandle(t), newPoolTestHandle(t)
	stubNumaNodes(t, numa.NumaNode{NodeId: 0, Cpulist: "0-3"})
	p, _ := newPoolForTest(t, v1, v2, 2, true)

	require.NoError(t, p.init())
	t.Cleanup(p.stop)

	require.Equal(t, []string{fmt.Sprintf("%s/numa0", handle.DefaultPathPoolV2)}, v2.recorded().cpus)
	require.True(t, p.poolV2.enabled)
	require.Len(t, p.poolV2.numaPools, 1)

	cg, err := p.Get(context.Background(), "sb-numa", true, 0)
	require.NoError(t, err)
	require.True(t, IsPoolV2ID(*cg))
}

// A single-NUMA host gains nothing from the per-NUMA pool, so the config check
// must answer false and the pool must pre-provision nothing.
func TestInitSkipsPoolV2OnSingleNumaHost(t *testing.T) {
	prev := numaNodeCount
	numaNodeCount = func() int { return 1 }
	t.Cleanup(func() { numaNodeCount = prev })

	cfg := &Config{EnableNumaAffinity: true}
	require.False(t, cfg.NumaAffinityEnabled())

	v1, v2 := newPoolTestHandle(t), newPoolTestHandle(t)
	p, _ := newPoolForTest(t, v1, v2, 2, cfg.NumaAffinityEnabled())

	require.NoError(t, p.init())
	t.Cleanup(p.stop)

	got := v2.recorded()
	require.Empty(t, got.cpus)
	require.Empty(t, got.subdirs)
	require.False(t, p.poolV2.enabled)
	require.Empty(t, p.poolV2.numaPools)
}

// The scenario the change has to keep working: the switch was on and a sandbox
// landed in the per-NUMA pool, then it is turned off and cubelet restarts while
// that sandbox still runs. It must keep its cgroup, cpuset and limits, stay
// visible to the reporting paths, survive Tidy, and only leave the bookkeeping
// when destroyed - possibly after the switch was turned back on.
func TestSwitchOffRestartKeepsRunningNumaSandbox(t *testing.T) {
	const sandboxID = "sb-numa"
	// numa1 slot 0, the layout the switch-on run would have handed out.
	numaSlot := MakeFullCgID(0, true, 1)
	numaPath := MakeCgroupPathByID(numaSlot)

	stubNumaNodes(t,
		numa.NumaNode{NodeId: 0, Cpulist: "0-3"},
		numa.NumaNode{NodeId: 1, Cpulist: "4-7"},
	)
	v1, v2 := newPoolTestHandle(t), newPoolTestHandle(t)

	// One store across all four restarts, as a real restart would keep.
	db, err := utils.NewCubeStoreExt(filepath.Join(t.TempDir(), "db"), "meta.db", 4, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// Phase 1: the switch is on, the sandbox is created and recorded.
	enabled := newPoolForTestWithStore(t, v1, v2, 2, true, db)
	require.NoError(t, enabled.init())
	cg, err := enabled.Get(context.Background(), sandboxID, true, 1)
	require.NoError(t, err)
	require.Equal(t, numaSlot, *cg)
	recordInUse(t, db, sandboxID, *cg)
	enabled.stop()
	v2.setExist(numaPath, true)

	// Phase 2: the operator turns the switch off and restarts cubelet.
	v1.reset()
	v2.reset()
	disabled := newPoolForTestWithStore(t, v1, v2, 2, false, db)
	require.NoError(t, disabled.init())
	t.Cleanup(disabled.stop)

	got := v2.recorded()
	require.Empty(t, got.cpus, "the disabled restart must not pre-create per-NUMA directories")
	require.Empty(t, got.subdirs, "the disabled restart must not walk the per-NUMA pool")
	require.Empty(t, got.deleted, "the disabled restart must not delete anything")
	require.Empty(t, got.cleaned, "the disabled restart must not clean anything")

	// The running sandbox is still accounted for, both as a slot and as a cgroup
	// that exists on the host.
	require.ElementsMatch(t, toInts([]uint32{numaSlot}), toInts(disabled.poolV2.All()))
	groups, err := disabled.GetAllGroupsExists()
	require.NoError(t, err)
	require.Contains(t, toInts(groups), int(numaSlot))

	// Its host metrics stay readable: the read routes by kind, not by the switch.
	usage, err := disabled.readUsageSnapshot(context.Background(), numaSlot)
	require.NoError(t, err)
	require.Equal(t, v2.usageValue, usage)

	// The maintenance loop must not touch it.
	require.Empty(t, disabled.Tidy())
	got = v2.recorded()
	require.Empty(t, got.deleted)
	require.Empty(t, got.subdirs)

	// New sandboxes go to the v1 pool, and only the v2 slot is a v2 slot.
	newCg, err := disabled.Get(context.Background(), "sb-new", false, 0)
	require.NoError(t, err)
	require.False(t, IsPoolV2ID(*newCg))
	require.ElementsMatch(t, toInts([]uint32{numaSlot}), toInts(disabled.poolV2.All()))

	// Phase 3: the operator turns the switch back on while the sandbox still
	// runs. The restored slot must not be handed out a second time.
	v1.reset()
	v2.reset()
	reenabled := newPoolForTestWithStore(t, v1, v2, 2, true, db)
	require.NoError(t, reenabled.init())
	t.Cleanup(reenabled.stop)

	require.Contains(t, toInts(reenabled.poolV2.All()), int(numaSlot),
		"the slot of the running sandbox must stay assigned")
	nextCg, err := reenabled.Get(context.Background(), "sb-numa-2", true, 1)
	require.NoError(t, err)
	require.NotEqual(t, numaSlot, *nextCg, "a running sandbox's slot must never be re-handed out")
	reenabled.stop()

	// Phase 4: the switch is off again and the sandbox is destroyed. The slot
	// leaves the bookkeeping without the cgroup being touched, and no baseline
	// is queued because the pool is not handed out.
	v1.reset()
	v2.reset()
	final := newPoolForTestWithStore(t, v1, v2, 2, false, db)
	require.NoError(t, final.init())
	t.Cleanup(final.stop)
	require.Contains(t, toInts(final.poolV2.All()), int(numaSlot))

	final.Put(context.Background(), numaSlot)

	got = v2.recorded()
	require.Empty(t, got.cleaned, "a disabled pool must not clean the slot it no longer manages")
	require.Empty(t, got.deleted, "a disabled pool must not delete a slot it no longer manages")
	require.Empty(t, got.created, "a destroyed sandbox's cgroup must not be recreated")
	require.NotContains(t, toInts(final.poolV2.All()), int(numaSlot), "the slot must be released")
	final.mutex.RLock()
	_, pending := final.pendingSet[numaSlot]
	_, cached := final.baselineCache[numaSlot]
	_, dirty := final.dirtySet[numaSlot]
	final.mutex.RUnlock()
	require.False(t, pending, "a disabled pool must not queue a baseline read")
	require.False(t, cached)
	require.False(t, dirty, "a disabled pool must not retry a clean it never started")
}

func toInts(ids []uint32) []int {
	out := make([]int, len(ids))
	for i, id := range ids {
		out[i] = int(id)
	}
	return out
}

// CollectMetric reads each slot's quota with the handle that owns its pool. Both
// handles resolve the same paths today, so this pins the routing.
func TestCollectMetricRoutesQuotaReadByPoolVersion(t *testing.T) {
	v1, v2 := newPoolTestHandle(t), newPoolTestHandle(t)
	v1.cpuQuota, v1.memQuota = 2, 1<<30
	v2.cpuQuota, v2.memQuota = 4, 2<<30

	p, db := newPoolForTest(t, v1, v2, 2, false)
	v1Slot := uint32(1)
	v2Slot := MakeFullCgID(0, true, 1)
	recordInUse(t, db, "sb-v1", v1Slot)
	recordInUse(t, db, "sb-numa", v2Slot)
	require.NoError(t, p.init())
	t.Cleanup(p.stop)
	v1.reset()
	v2.reset()

	plugin := &CgPlugin{poolV1Handle: v1, poolV2Handle: v2, pool: p}
	metrics := plugin.CollectMetric(context.Background())

	require.Equal(t, 2, metrics.MvmNum)
	require.Equal(t, v1.cpuQuota+v2.cpuQuota, metrics.QuotaCpuUsage)
	require.Equal(t, int64(v1.memQuota+v2.memQuota)>>20, metrics.QuotaMemMbUsage)

	v1Group := MakeCgroupPathByID(v1Slot)
	v2Group := MakeCgroupPathByID(v2Slot)
	require.Subset(t, v1.recorded().quotaRead, []string{v1Group})
	require.ElementsMatch(t, []string{v2Group, v2Group}, v2.recorded().quotaRead,
		"a legacy v2 slot must be read with the v2 handle (cpu and memory)")
	require.NotContains(t, v1.recorded().quotaRead, v2Group,
		"the v1 handle must never be asked about a per-NUMA slot")
}

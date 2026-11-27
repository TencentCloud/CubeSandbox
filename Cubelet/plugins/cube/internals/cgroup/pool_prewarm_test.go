// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cgroup

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/allocator"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/numa"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

func stubPrewarmNodes(t *testing.T, count int) {
	t.Helper()
	nodes := make([]numa.NumaNode, count)
	// Sysfs order must not decide which node receives the remainder.
	for i := range nodes {
		id := count - 1 - i
		nodes[i] = numa.NumaNode{NodeId: id, Cpulist: strconv.Itoa(id)}
	}
	stubNumaNodes(t, nodes...)
}

func TestPrewarmNodeWideBudgetAndSkewedGrowth(t *testing.T) {
	for _, budget := range []int{0, 1, 3, 10} {
		t.Run(strconv.Itoa(budget), func(t *testing.T) {
			stubPrewarmNodes(t, 4)
			v1, v2 := newPoolTestHandle(t), newPoolTestHandle(t)
			p, _ := newPoolForTest(t, v1, v2, budget, true)
			require.NoError(t, p.init())
			t.Cleanup(p.stop)
			require.Empty(t, v1.recorded().created)
			require.Nil(t, p.poolV1.cgRanger)
			require.Len(t, v2.recorded().created, budget)
			require.Len(t, v2.recorded().cpus, 4, "zero-share nodes still need parents for on-demand growth")
			for node, pool := range p.poolV2.numaPools {
				want := budget / 4
				if node < budget%4 {
					want++
				}
				require.Equal(t, want, pool.prewarmSize)
				require.Equal(t, want, pool.cgRanger.Cap())
			}
			for id := 0; id < 5; id++ {
				cg, err := p.Get(context.Background(), fmt.Sprintf("skewed-%d", id), true, 3)
				require.NoError(t, err)
				require.Equal(t, MakeFullCgID(uint16(id), true, 3), *cg)
			}
		})
	}
}

func TestActiveRecoveryPrewarmsNoRangeHoles(t *testing.T) {
	for _, activeV2 := range []bool{false, true} {
		t.Run(fmt.Sprint(activeV2), func(t *testing.T) {
			stubPrewarmNodes(t, 4)
			v1, v2 := newPoolTestHandle(t), newPoolTestHandle(t)
			p, db := newPoolForTest(t, v1, v2, 3, activeV2)
			id := MakeFullCgID(40, activeV2, 2)
			recordInUse(t, db, "recovered", id)
			require.NoError(t, p.init())
			t.Cleanup(p.stop)
			h := v1
			warm := 4
			if activeV2 {
				h = v2
				warm = 3
			}
			require.Len(t, h.recorded().created, warm+1, "only the warm range and actual recovered ID are created")
			require.Contains(t, h.recorded().created, MakeCgroupPathByID(id))
			require.Contains(t, p.All(), id)
		})
	}
}

func TestBaselineSweepOnlyPrewarmedSlots(t *testing.T) {
	for _, activeV2 := range []bool{false, true} {
		t.Run(fmt.Sprint(activeV2), func(t *testing.T) {
			stubPrewarmNodes(t, 4)
			p := &cgPool{initialSize: 3, queue: newIDQueue()}
			var used uint32
			if activeV2 {
				require.NoError(t, p.initPoolV2())
				used = MakeFullCgID(0, true, 0)
				require.NoError(t, p.poolV2.initialAssign(used))
				require.NoError(t, p.poolV2.initialAssign(MakeFullCgID(40, true, 2)))
			} else {
				require.NoError(t, p.initPoolV1())
				require.NoError(t, p.poolV1.initialAssign(0))
				require.NoError(t, p.poolV1.initialAssign(40))
			}
			p.sweepBaselines(map[uint32]struct{}{used: {}})
			want := []uint32{1, 2, 3}
			if activeV2 {
				want = []uint32{MakeFullCgID(0, true, 1), MakeFullCgID(0, true, 2)}
			}
			var got []uint32
			for {
				id, ok := p.queue.tryPop()
				if !ok {
					break
				}
				got = append(got, id)
			}
			require.ElementsMatch(t, want, got)
		})
	}
}

func TestSwitchOnRestartKeepsRunningPoolV1Sandbox(t *testing.T) {
	stubPrewarmNodes(t, 2)
	v1, v2 := newPoolTestHandle(t), newPoolTestHandle(t)
	p, db := newPoolForTest(t, v1, v2, 0, false)
	recordInUse(t, db, "legacy-v1", 40)
	require.NoError(t, p.init())
	t.Cleanup(p.stop)
	require.Contains(t, p.All(), uint32(40))

	// Fresh handles isolate the disabled phase from workers of the previous run.
	old, active := newPoolTestHandle(t), newPoolTestHandle(t)
	old.setExist(MakeCgroupPathByID(40), true)
	switched := newPoolForTestWithStore(t, old, active, 2, true, db)
	require.NoError(t, switched.init())
	t.Cleanup(switched.stop)
	require.False(t, switched.poolV1.enabled)
	require.Nil(t, switched.poolV1.allocator)
	require.Contains(t, switched.All(), uint32(40))
	groups, err := switched.GetAllGroupsExists()
	require.NoError(t, err)
	require.Contains(t, groups, uint32(40), "inactive assignments stay visible without listing the old pool")
	require.Empty(t, switched.Tidy())
	_, err = switched.Get(context.Background(), "wrong-pool", false, 0)
	require.ErrorContains(t, err, "not enabled")
	switched.Put(context.Background(), 40)
	require.NotContains(t, switched.All(), uint32(40))
	switched.mutex.RLock()
	pending, dirty := len(switched.pendingSet), len(switched.dirtySet)
	switched.mutex.RUnlock()
	require.Zero(t, pending)
	require.Zero(t, dirty)
	calls := old.recorded()
	require.Empty(t, calls.created)
	require.Empty(t, calls.cpus)
	require.Empty(t, calls.cleaned)
	require.Empty(t, calls.deleted)
	require.Empty(t, calls.usage)

	// Re-enabling poolV1 reserves the DB ID again; it cannot be handed out.
	nextV1, nextV2 := newPoolTestHandle(t), newPoolTestHandle(t)
	restored := newPoolForTestWithStore(t, nextV1, nextV2, 0, false, db)
	require.NoError(t, restored.init())
	t.Cleanup(restored.stop)
	require.Contains(t, restored.poolV1.All(), uint32(40))
	for i := 0; i < 41; i++ {
		cg, err := restored.Get(context.Background(), fmt.Sprint(i), false, 0)
		require.NoError(t, err)
		require.NotEqual(t, uint32(40), *cg)
	}
}

func TestGetRepairsExistingPoolV2SlotCpuset(t *testing.T) {
	for _, target := range []uint16{0, 2, 5} {
		t.Run(fmt.Sprint(target), func(t *testing.T) {
			dir := newSlotDir(t)
			require.NoError(t, applyCpusetFiles(dir, "0", 1))
			group := MakeCgroupPathByID(MakeFullCgID(target, true, 0))
			h := &cpusetTestHandle{dirs: map[string]string{group: dir}}
			p := newCpusetTestPool(t, h)
			r := allocator.NewEmptySimpleLinearRanger()
			r.ExpandTo(40)
			a := allocator.NewAllocator[uint16](r)
			require.NoError(t, a.Assign(40))
			for id := uint16(0); id < 40; id++ {
				if id != target {
					require.NoError(t, a.Assign(id))
				}
			}
			p.poolV2.numaPools[0].cgRanger = r
			p.poolV2.numaPools[0].allocator = a
			if target == 0 {
				p.poolV2.numaPools[0].prewarmSize = 0
			}

			cg, err := p.Get(context.Background(), "reuse", true, 0)
			require.NoError(t, err)
			require.Equal(t, MakeFullCgID(target, true, 0), *cg)
			require.Equal(t, "0-3", readCpusetFile(t, dir, "cpuset.cpus"))
			require.Equal(t, "0", readCpusetFile(t, dir, "cpuset.mems"))
		})
	}
}

// A permanently unrepairable slot still serves with its previous mask, just
// as at startup. Keeping its ID allocated lets the next request grow the pool.
func TestGetCpusetRepairFailureDoesNotBlockGrowth(t *testing.T) {
	dir := newSlotDir(t)
	require.NoError(t, applyCpusetFiles(dir, "0", 0))
	h := &cpusetTestHandle{dir: dir, dirErr: errors.New("cpuset repair unavailable")}
	p := newCpusetTestPool(t, h)
	r, err := newCgroupRanger(1)
	require.NoError(t, err)
	p.poolV2.numaPools[0].cgRanger = r
	p.poolV2.numaPools[0].allocator = allocator.NewAllocator[uint16](r)
	for id := uint16(0); id < 3; id++ {
		cg, err := p.Get(context.Background(), fmt.Sprint(id), true, 0)
		require.NoError(t, err)
		require.Equal(t, MakeFullCgID(id, true, 0), *cg)
		require.True(t, p.poolV2.numaPools[0].allocator.Has(id))
	}
	require.Equal(t, 3, r.Cap(), "repair failure must not suppress on-demand expansion")
	require.Equal(t, "0", readCpusetFile(t, dir, "cpuset.cpus"))
	require.Equal(t, "0", readCpusetFile(t, dir, "cpuset.mems"))
}

func TestPrewarmCapacityBounds(t *testing.T) {
	for _, size := range []int{0, 1, CgPoolIdLimit} {
		r, err := newCgroupRanger(size)
		require.NoError(t, err)
		require.Equal(t, size, r.Cap())
	}
	for _, size := range []int{-1, CgPoolIdLimit + 1} {
		_, err := newCgroupRanger(size)
		require.Error(t, err)
	}
	for _, size := range []int{-1, CgPoolIdLimit} {
		p := &cgPool{initialSize: size}
		require.Error(t, p.initPoolV1())
	}
	p := &cgPool{initialSize: CgPoolIdLimit - 1}
	require.NoError(t, p.initPoolV1())
	require.Equal(t, CgPoolIdLimit, p.poolV1.prewarmSize)
	require.Equal(t, CgPoolIdLimit, p.poolV1.cgRanger.Cap())
	stubPrewarmNodes(t, 4)
	p = &cgPool{initialSize: CgPoolIdLimit + 1}
	require.NoError(t, p.initPoolV2())
	total := 0
	for _, node := range p.poolV2.numaPools {
		total += node.cgRanger.Cap()
	}
	require.Equal(t, CgPoolIdLimit+1, total)
	p.initialSize = 4*CgPoolIdLimit + 1
	require.Error(t, p.initPoolV2())
	p.initialSize = -1
	require.Error(t, p.initPoolV2())
}

func TestTidyPoolV1RetainsHistoricalSlots(t *testing.T) {
	v1, v2 := newPoolTestHandle(t), newPoolTestHandle(t)
	p, _ := newPoolForTest(t, v1, v2, 0, false)
	require.NoError(t, p.init())
	t.Cleanup(p.stop)
	for id := 1; id <= 10; id++ {
		require.NoError(t, v1.Create(context.Background(), MakeCgroupPathByID(uint32(id))))
	}
	// Invalid v1 names must not be interpreted as dirty poolV2 IDs.
	require.NoError(t, v1.Create(context.Background(), handle.DefaultPathPoolV1+"/65536"))
	p.mutex.Lock()
	p.dirtySet[MakeFullCgID(0, true, 0)] = struct{}{}
	p.mutex.Unlock()
	require.Empty(t, p.Tidy())
	require.Equal(t, []string{handle.DefaultPathPoolV1 + "/65536"}, v1.recorded().deleted,
		"valid historical slots must not enter forced deletion")
	require.Empty(t, v1.recorded().cleaned)
	require.Empty(t, v2.recorded().cleaned)
}

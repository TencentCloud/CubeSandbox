// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/allocator"
	dynamConf "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/numa"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

const CgPoolIdLimit = math.MaxUint16 + 1
const CgPoolV1IdLimit = CgPoolIdLimit

func isNotExistError(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "no such file or directory") ||
		strings.Contains(err.Error(), "not exist"))
}

type cgPool struct {
	dirtySet map[uint32]struct{}
	mutex    sync.RWMutex

	initialSize int

	poolV1Handle handle.Interface
	poolV2Handle handle.Interface
	db           *utils.CubeStore

	poolV1 cgPoolV1
	poolV2 cgPoolV2

	individualGroups map[string]struct{}

	// Return-time baseline state: pendingSet holds cleaned slots awaiting
	// their baseline read (invisible to the allocator until then);
	// baselineCache holds idle-slot counters, popped at assignment.
	pendingSet      map[uint32]struct{}
	baselineCache   map[uint32]handle.UsageSnapshot
	queue           *idQueue
	stopCh          chan struct{}
	stopOnce        sync.Once
	workers         int
	triggerInterval time.Duration
	// Injected closure; the dynamic config watcher can flip the value at
	// runtime.
	reparentFileEnabled func() bool
	// Injected closure; reports whether the per-NUMA pool is in use, which
	// decides how strictly the pool bootstrap treats a broken slot. Unlike
	// reparentFileEnabled above this one cannot change at runtime:
	// enable_numa_affinity is read from config.toml once at plugin init and the
	// dynamic config watcher never looks at it. It is a closure only so tests
	// can drive both modes.
	numaAffinityInUse func() bool
}

func (p *cgPool) addIndividualCgroup(group ...string) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	for _, g := range group {
		p.individualGroups[g] = struct{}{}
	}
}

func (p *cgPool) createCgroup(ctx context.Context, fullCgID uint32) error {
	var handle handle.Interface
	var err error
	group := MakeCgroupPathByID(fullCgID)

	if IsPoolV2ID(fullCgID) {
		handle = p.poolV2Handle
	} else {
		handle = p.poolV1Handle
	}

	exists := handle.IsExist(ctx, group)
	if exists {
		return nil
	}
	log.G(ctx).Debugf("cgroup %s not exist, create it", group)
	err = handle.Create(ctx, group)
	if err != nil {
		return fmt.Errorf("create cgroup %s error: %w", group, err)
	}

	// Best effort: a disabled config still writes 0 to any existing
	// reparent file; failures do not fail creation.
	_ = setupMemoryReparentFileFn(ctx, fullCgID, p.reparentFileEnabled())

	return nil
}

func (p *cgPool) init() error {

	allUse, err := p.db.ReadAll(bucket)
	if err != nil {
		return fmt.Errorf("recover cg data from db: %w", err)
	}
	inUse := make(map[uint32]struct{}, len(allUse))

	p.mutex.Lock()
	p.dirtySet = make(map[uint32]struct{})
	p.individualGroups = make(map[string]struct{})
	p.pendingSet = make(map[uint32]struct{})
	p.baselineCache = make(map[uint32]handle.UsageSnapshot)
	p.queue = newIDQueue()
	p.stopCh = make(chan struct{})
	if p.workers < 1 {
		p.workers = 1
	}
	if p.triggerInterval <= 0 {
		p.triggerInterval = time.Duration(defaultPoolTriggerIntervalInMs) * time.Millisecond
	}
	err = p.initPoolV1()
	if err != nil {
		p.mutex.Unlock()
		return fmt.Errorf("init pool v1 failed: %w", err)
	}

	// The per-NUMA pool is bootstrapped only when Create can hand it out;
	// otherwise the pre-created slots would never be used.
	poolV2Enabled := p.isPoolV2Enabled()
	p.poolV2.enabled = poolV2Enabled
	if poolV2Enabled {
		if err := p.initPoolV2(); err != nil {
			p.mutex.Unlock()
			return fmt.Errorf("init pool v2 failed: %w", err)
		}
	}

	var inUseV2 []uint32
	for _, inuseCgIdBytes := range allUse {
		inuseCgIdInt, err := strconv.Atoi(string(inuseCgIdBytes))
		if err != nil || inuseCgIdInt < 0 {
			continue
		}
		inUse[uint32(inuseCgIdInt)] = struct{}{}
		if inuseCgIdInt < CgPoolV1IdLimit {
			p.poolV1.initialAssign(uint32(inuseCgIdInt))
		} else if poolV2Enabled {
			p.poolV2.initialAssign(uint32(inuseCgIdInt))
		} else {
			inUseV2 = append(inUseV2, uint32(inuseCgIdInt))
		}
	}
	for _, fullCgID := range inUseV2 {
		p.poolV2.trackLegacy(fullCgID)
	}
	poolV1ActualSize := p.poolV1.cgRanger.Cap()
	p.mutex.Unlock()

	if !poolV2Enabled && len(inUseV2) > 0 {
		log.G(context.Background()).Infof("cgroup pool v2 disabled: skipping pre-provisioning; "+
			"keeping bookkeeping for %d sandbox slot(s) created by a previous run", len(inUseV2))
	}

	err = p.initPoolV1Late(poolV1ActualSize)
	if err != nil {
		return fmt.Errorf("init pool v1 late failed: %w", err)
	}

	if poolV2Enabled {
		if err := p.initPoolV2Late(); err != nil {
			return fmt.Errorf("init pool v2 late failed: %w", err)
		}
	}

	p.sweepBaselines(inUse)
	p.start()
	return nil
}

func (p *cgPool) initPoolV1() error {
	cgRanger, err := allocator.NewSimpleLinearRanger(0, uint16(p.initialSize))
	if err != nil {
		return fmt.Errorf("create cg ranger error: %w", err)
	}
	alloc := allocator.NewAllocator[uint16](cgRanger)
	p.poolV1.allocator = alloc
	p.poolV1.cgRanger = cgRanger
	p.poolV1.handle = p.poolV1Handle

	return nil
}

func (p *cgPool) initPoolV1Late(poolV1ActualSize int) error {

	ctx := context.Background()
	for cgId := 0; cgId < poolV1ActualSize; cgId++ {
		if err := p.createCgroup(ctx, uint32(cgId)); err != nil {
			log.G(ctx).Errorf("cgroup pool v1 init: %v", err)
		} else {
			_ = setupMemoryReparentFileFn(ctx, uint32(cgId), p.reparentFileEnabled())
		}
	}

	return nil
}

// numaNodeCount and getAllNumaNodes are variables rather than direct calls
// because pkg/numa is not injectable: the single-NUMA fallback and the >= 11
// node ordering (see initPoolV2) are the two things worth pinning in a test
// without a NUMA host.
var (
	numaNodeCount   = numa.GetNumaNodeCount
	getAllNumaNodes = numa.GetAllNumaNodes
)

// NumaAffinityEnabled reports whether the per-NUMA cgroup pool should be used.
// The v2 pool keeps one subtree per NUMA node (numa<N>) whose cpuset.cpus and
// cpuset.mems carry that node's cpulist / node id; a sandbox cgroup created
// under it inherits the binding, which is what turns the NUMA node picked for
// the sandbox into a real CPU + memory affinity constraint.
//
// Inheritance is what makes that work on both backends: the unified hierarchy
// hands a child the effective parent mask, and on cgroup v1 containerd's cpuset
// controller copies cpuset.cpus / cpuset.mems into each new child (copyIfNeeded)
// because the slots are created with an empty CPU spec.
//
// Opt-in via enable_numa_affinity. A single-NUMA host gains nothing from the
// v2 pool, so the setting is ignored there and the v1 pool stays in use. The
// count is the one from the NUMA sysfs, so a node without CPUs counts as well:
// a host with one CPU node and one memory-only (CXL / PMEM) node passes this
// check and is then rejected by the bootstrap, because that node cannot be
// bound and the switch has no per-node opt-out (see initPoolV2Late).
func (c *Config) NumaAffinityEnabled() bool {
	if c == nil || !c.EnableNumaAffinity {
		return false
	}
	return numaNodeCount() > 1
}

// isPoolV2Enabled reports whether this pool hands out per-NUMA slots. The value
// is fixed at plugin init, so tests inject it through numaAffinityInUse.
func (p *cgPool) isPoolV2Enabled() bool {
	return p.numaAffinityInUse != nil && p.numaAffinityInUse()
}

func (p *cgPool) initPoolV2() error {
	numaNodes := getAllNumaNodes()
	maxNumaNodeID := 0
	for _, node := range numaNodes {
		if node.NodeId > maxNumaNodeID {
			maxNumaNodeID = node.NodeId
		}
	}
	nodeCount := maxNumaNodeID + 1
	p.poolV2.handle = p.poolV2Handle
	p.poolV2.numaPools = make([]cgPoolV2Numa, nodeCount)

	// Index by nd.NodeId, never by the position in numaNodes: GetAllNumaNodes
	// reads /sys/devices/system/node with os.ReadDir, which sorts
	// lexicographically, so on a host with 11+ nodes the slice order is
	// node0, node1, node10, node11, node2, ... Using the position would pair a
	// slot's ranger with another node's directory, and tidyPoolV2 range-checks
	// the directories it lists against that ranger - so it would delete in-use
	// slots of the wrong node once a node grows past the initial pool size.
	for _, nd := range numaNodes {
		cgRanger, err := allocator.NewSimpleLinearRanger(0, uint16(p.initialSize))
		if err != nil {
			return fmt.Errorf("create cg ranger error: %w", err)
		}
		alloc := allocator.NewAllocator[uint16](cgRanger)
		p.poolV2.numaPools[nd.NodeId].allocator = alloc
		p.poolV2.numaPools[nd.NodeId].cgRanger = cgRanger
		p.poolV2.numaPools[nd.NodeId].numaNode = nd
	}

	return nil
}

// ensureNumaSlotCpuset (re)applies a node's cpulist / node id to the node
// directory of the per-NUMA pool (numa<N>). The directory outlives a cubelet
// restart, and creating an existing cgroup is a no-op or an error depending on
// the driver, so the values are re-asserted on every startup; this also repairs
// the node directory whose mask drifted (CPU hot-add, manual change). The
// sandbox slots below it are a separate matter, see ensureSandboxSlotCpuset.
//
// The directory is resolved by the handle, so a cgroup v1 host with the cpuset
// controller co-mounted (cpu,cpuacct,cpuset) or on a non-default root is handled
// too, and a controller that is not available is an error instead of a silent
// no-op. On the unified hierarchy the handle points at the cgroup itself, so
// the same re-assert is what keeps the node's mask current.
func ensureNumaSlotCpuset(h handle.Interface, slot, cpulist string, nodeID int) error {
	dir, err := h.CpusetDir(slot)
	if err != nil {
		return err
	}
	return applyCpusetFiles(dir, cpulist, nodeID)
}

// ensureSandboxSlotCpuset repairs the mask of one per-sandbox slot under a node
// directory. On cgroup v1 a slot takes a copy of the parent mask when it is
// created (containerd's copyIfNeeded) and createCgroup returns early for a slot
// that already exists, so one that predates a CPU hot-add - or any widening of
// the node mask - keeps the old cpuset, and a sandbox placed on it silently
// misses the new CPUs. The unified hierarchy needs nothing here: an unset child
// tracks its parent, and cpuset.cpus may not even be exposed on the slot, in
// which case there is nothing to repair.
func ensureSandboxSlotCpuset(h handle.Interface, group, cpulist string, nodeID int) error {
	dir, err := h.CpusetDir(group)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "cpuset.cpus")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return applyCpusetFiles(dir, cpulist, nodeID)
}

// parseCPUList parses a cpuset list ("0-3,8,10-11") into the set of ids it
// names. An empty list is an empty set, which is what a slot that was never
// written reads back as.
func parseCPUList(list string) (map[int]struct{}, error) {
	ids := make(map[int]struct{})
	list = strings.TrimSpace(list)
	if list == "" {
		return ids, nil
	}
	for _, part := range strings.Split(list, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		start, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("parse cpu list %q: %w", list, err)
		}
		end := start
		if isRange {
			if end, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
				return nil, fmt.Errorf("parse cpu list %q: %w", list, err)
			}
		}
		if end < start {
			return nil, fmt.Errorf("parse cpu list %q: %q ends before it starts", list, part)
		}
		for id := start; id <= end; id++ {
			ids[id] = struct{}{}
		}
	}
	return ids, nil
}

// sameCPUSet reports whether two cpuset lists name the same ids. cgroupfs
// returns the effective set rather than the text it was given, so the
// comparison has to be on parsed sets; comparing text alone would reject a
// slot that is bound correctly, which with the switch on fails startup.
//
// The text check first is only an optimization for the common case, not a
// relaxation: equal text always means equal sets.
func sameCPUSet(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == b {
		return true
	}
	return sameCPUSetIDs(a, b)
}

// sameCPUSetIDs is the exact comparison: both lists are parsed into sets of ids.
func sameCPUSetIDs(a, b string) bool {
	left, err := parseCPUList(a)
	if err != nil {
		return false
	}
	right, err := parseCPUList(b)
	if err != nil {
		return false
	}
	if len(left) != len(right) {
		return false
	}
	for id := range left {
		if _, ok := right[id]; !ok {
			return false
		}
	}
	return true
}

// applyCpusetFiles makes dir carry exactly cpulist / nodeID, and returns nil
// only when the written values read back as expected, so a nil return means the
// mask is what a child cgroup will inherit rather than "the write was accepted".
func applyCpusetFiles(dir, cpulist string, nodeID int) error {
	names := [2]string{"cpuset.cpus", "cpuset.mems"}
	want := [2]string{cpulist, strconv.Itoa(nodeID)}
	var wrote [2]bool

	for i := 0; i < len(names); i++ {
		file := path.Join(dir, names[i])
		cur, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("read %s: %w", file, err)
		}
		if sameCPUSet(string(cur), want[i]) {
			continue
		}
		if err := os.WriteFile(file, []byte(want[i]), 0644); err != nil {
			return fmt.Errorf("write %s=%s: %w", names[i], want[i], err)
		}
		wrote[i] = true
	}

	// cgroupfs can accept a write and keep something else, so only the written
	// files are read back; a value that matched on the first read is proven.
	for i := 0; i < len(names); i++ {
		if !wrote[i] {
			continue
		}
		file := path.Join(dir, names[i])
		cur, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("read back %s: %w", file, err)
		}
		if got := strings.TrimSpace(string(cur)); !sameCPUSet(got, want[i]) {
			return fmt.Errorf("%s reads back %q, want %q", file, got, want[i])
		}
	}
	return nil
}

// initPoolV2Late creates and validates the per-NUMA pool: one node directory per
// node plus initialSize slots under it. Called only with the switch on, so any
// failure is fatal - a node reporting ready with an unbound pool would place
// every sandbox there without affinity.
func (p *cgPool) initPoolV2Late() error {
	ctx := context.Background()
	for i := 0; i < len(p.poolV2.numaPools); i++ {
		numaPool := &p.poolV2.numaPools[i]
		numaNode := numaPool.numaNode
		log.G(ctx).Infof("cgroup pool v2 init: nodeId: %d, cpus: %s", numaNode.NodeId, numaNode.Cpulist)
		if numaNode.Cpulist == "" {
			// An empty mask is accepted by the kernel but binds nothing (v1:
			// children inherit an empty cpuset; v2: empty means "inherit" the
			// root mask). Reachable on a memory-only node (CXL/PMEM) or when
			// every CPU is in cgroup_disable_cpuset_list. The switch is host-wide
			// with no per-node opt-out, so such a host has to leave it off.
			return fmt.Errorf("cgroup pool v2 init: numa%d has no cpus to bind (cpulist is empty; "+
				"enable_numa_affinity has to stay off on a host with such a node)", numaNode.NodeId)
		}
		slot := fmt.Sprintf("%s/numa%d", handle.DefaultPathPoolV2, numaNode.NodeId)
		createErr := p.poolV2.handle.CreateWithCpuSet(ctx, slot, numaNode.Cpulist, numaNode.NodeId)
		applyErr := ensureNumaSlotCpuset(p.poolV2.handle, slot, numaNode.Cpulist, numaNode.NodeId)
		// A slot that cannot be proven to carry the node mask must fail startup:
		// the pool is what Create hands out.
		if applyErr != nil {
			// Keep createErr too: it is usually what explains the failed apply.
			if createErr != nil {
				return fmt.Errorf("cgroup pool v2 init: numa%d slot %s: %w",
					numaNode.NodeId, slot, errors.Join(createErr, applyErr))
			}
			return fmt.Errorf("cgroup pool v2 init: numa%d slot %s: apply cpuset: %w",
				numaNode.NodeId, slot, applyErr)
		}
		// The mask was written and read back, so the slot is bound and a create
		// error alone is tolerated: on v1 creating an existing cgroup errors, and
		// that is just the slot of a previous run. A v2 slot left half-created
		// (controllers not enabled) also lands here; it surfaces on the first
		// sandbox, in SetCubeboxCgroupLimit.
		if createErr != nil {
			log.G(ctx).Warnf("cgroup pool v2 init: numa%d slot create: %v", numaNode.NodeId, createErr)
		}
		cap := uint16(numaPool.cgRanger.Cap())
		var slotMaskFails int
		var firstSlotMaskErr error
		for shortCgId := uint16(0); shortCgId < cap; shortCgId++ {
			fullCgID := MakeFullCgID(shortCgId, true, int32(numaNode.NodeId))
			if err := p.createCgroup(ctx, fullCgID); err != nil {
				log.G(ctx).Errorf("cgroup pool v2 init: create fullCgID: %d failed: %v", fullCgID, err)
				continue
			}
			_ = setupMemoryReparentFileFn(ctx, fullCgID, p.reparentFileEnabled())
			// Repair slots created before a CPU hot-add, which would otherwise
			// silently miss the new CPUs. Best effort: one failed patch does not
			// make the whole pool useless, so it is counted and logged once.
			if err := ensureSandboxSlotCpuset(p.poolV2.handle, MakeCgroupPathByID(fullCgID),
				numaNode.Cpulist, numaNode.NodeId); err != nil {
				slotMaskFails++
				if firstSlotMaskErr == nil {
					firstSlotMaskErr = err
				}
			}
		}
		if slotMaskFails > 0 {
			log.G(ctx).Warnf("cgroup pool v2 init: numa%d: %d sandbox slot(s) kept their previous cpuset: %v",
				numaNode.NodeId, slotMaskFails, firstSlotMaskErr)
		}

	}

	return nil
}

func (p *cgPool) Get(ctx context.Context, sandboxID string, usePoolV2 bool, numaNode int32) (*uint32, error) {
	var cg *uint16

	var err error
	if usePoolV2 {
		cg, err = p.poolV2.Get(ctx, sandboxID, numaNode)
	} else {
		cg, err = p.poolV1.Get(ctx, sandboxID)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to allocate cgroup: %w", err)
	}

	fullCgID := MakeFullCgID(*cg, usePoolV2, numaNode)

	if err := p.createCgroup(ctx, fullCgID); err != nil {
		p.allocatorReleaseCgroup(fullCgID)
		return nil, fmt.Errorf("failed to create cgroup %v in slow path: %w", cg, err)
	}

	return &fullCgID, nil

}

func IsPoolV2ID(fullCgID uint32) bool {
	return fullCgID >= CgPoolV1IdLimit
}

// Put cleans a returned cgroup synchronously, then defers its release until the
// background baseline read completes. A per-NUMA slot returned while the pool is
// disabled is only dropped from the bookkeeping; its cgroup is left as it is.
func (p *cgPool) Put(ctx context.Context, fullCgID uint32) {
	if IsPoolV2ID(fullCgID) && !p.isPoolV2Enabled() {
		p.allocatorReleaseCgroup(fullCgID)
		return
	}

	group := MakeCgroupPathByID(fullCgID)
	var err error
	if IsPoolV2ID(fullCgID) {
		err = p.poolV2Handle.CleanForReuse(ctx, group)
	} else {
		err = p.poolV1Handle.CleanForReuse(ctx, group)
	}

	p.mutex.Lock()
	if err == nil {
		delete(p.dirtySet, fullCgID)
		p.pendingSet[fullCgID] = struct{}{}
	} else {
		p.dirtySet[fullCgID] = struct{}{}
		log.G(ctx).Errorf("failed to clean cgroup %v for reuse: %v", MakeCgroupPathByID(fullCgID), err)
	}
	p.mutex.Unlock()

	if err == nil {
		p.queue.push(fullCgID)
	}
}

func (p *cgPool) allocatorReleaseCgroup(fullCgID uint32) {
	if IsPoolV2ID(fullCgID) {
		p.poolV2.allocatorRelease(fullCgID)
	} else {
		p.poolV1.allocatorRelease(uint16(fullCgID))
	}
}

func (p *cgPool) Tidy() []error {
	dirtyWork := make(map[uint32]struct{})
	p.mutex.RLock()
	for k, v := range p.dirtySet {
		dirtyWork[k] = v
	}
	p.mutex.RUnlock()

	errors := make([]error, 0)
	if err := p.tidyPoolV1(dirtyWork); err != nil {
		errors = append(errors, fmt.Errorf("cgroup tidy pool v1 error:%w", err))
	}
	if err := p.tidyPoolV2(dirtyWork); err != nil {
		errors = append(errors, fmt.Errorf("cgroup tidy pool v2 error:%w", err))
	}

	return errors
}

func (p *cgPool) tidyPoolV1(dirtyWork map[uint32]struct{}) error {
	all, err := p.poolV1Handle.List()
	if err != nil {

		if isNotExistError(err) {
			CubeLog.Warnf("cgroup tidy pool v1: cgroup directory not exist, may have been deleted: %v", err)
			return nil
		}
		return fmt.Errorf("cgroup tidy get all cgroup error:%s", err)
	}

	var toDelete []string
	for _, cg := range all {
		if _, ok := p.individualGroups[cg]; ok {
			continue
		}
		id, err := strconv.Atoi(cg)
		if err != nil {
			path := MakeCgroupPoolV1PathByString(cg)

			toDelete = append(toDelete, path)
			continue
		}
		cgId := uint32(id)

		if _, isDirty := dirtyWork[cgId]; isDirty {

			p.Put(context.Background(), cgId)
			continue
		}
		if p.poolV1.cgRanger.Contains(uint16(id)) {
			continue
		}
		path := MakeCgroupPoolV1PathByString(cg)

		toDelete = append(toDelete, path)
	}

	for _, cg := range toDelete {
		CubeLog.Errorf("clean unknown cg %s", cg)
		if err := p.poolV1Handle.Delete(context.Background(), cg); err != nil {
			CubeLog.Errorf("cgroup tidy delete cg %s error: %s", cg, err)
		}
	}

	return nil
}

// tidyPoolV2 retries dirty slots and deletes unknown directories of the per-NUMA
// pool. While the pool is disabled there is nothing to do: Put never marks a v2
// slot dirty in that mode, and the subtree is not ours to walk.
func (p *cgPool) tidyPoolV2(dirtyWork map[uint32]struct{}) error {
	if !p.isPoolV2Enabled() {
		return nil
	}

	for _, numaPool := range p.poolV2.numaPools {
		cgPath := numaPool.cgroupPath()
		all, err := p.poolV2Handle.ListSubdir(cgPath)
		if err != nil {
			return fmt.Errorf("cgroup tidy pool v2 get all cgroup error:%s", err)
		}
		var toDelete []string
		for _, cg := range all {
			if _, ok := p.individualGroups[cg]; ok {
				continue
			}
			id, err := strconv.Atoi(cg)
			if err != nil || id >= CgPoolIdLimit {
				path := MakeCgroupPoolV2PathByString(path.Join(cgPath, cg))

				toDelete = append(toDelete, path)
				continue
			}

			fullCgId := MakeFullCgID(uint16(id), true, int32(numaPool.numaNode.NodeId))
			if _, isDirty := dirtyWork[fullCgId]; isDirty {

				p.Put(context.Background(), fullCgId)
				continue
			}

			if numaPool.cgRanger.Contains(uint16(id)) {
				continue
			}

			path := MakeCgroupPoolV2PathByString(path.Join(cgPath, cg))

			toDelete = append(toDelete, path)
		}

		for _, path := range toDelete {
			CubeLog.Errorf("tidyPoolV2: clean unknown cg %s", path)
			if err := p.poolV2Handle.Delete(context.Background(), path); err != nil {
				CubeLog.Errorf("tidyPoolV2: cgroup tidy delete cg %s error: %s", path, err)
			}
		}

	}

	return nil
}

func (p *cgPool) All() []uint32 {
	allV1 := p.poolV1.All()
	allV2 := p.poolV2.All()
	return append(allV1, allV2...)
}

func (p *cgPool) GetAllGroupsExists() ([]uint32, error) {
	allV1, err := p.poolV1.AllGroups()
	if err != nil {
		return nil, fmt.Errorf("failed to get all groups from pool v1: %w", err)
	}
	allV2, err := p.poolV2.AllGroups()
	if err != nil {
		return nil, fmt.Errorf("failed to get all groups from pool v2: %w", err)
	}
	all := make([]uint32, 0, len(allV1)+len(allV2))
	all = append(all, allV1...)
	all = append(all, allV2...)
	return all, nil
}

type cgPluginConfigWatcher struct {
	lock                         sync.Mutex
	memReparentFileGoRoutineLock sync.Mutex
}

func (cw *cgPluginConfigWatcher) OnEvent(conf *dynamConf.Config) {
	cw.lock.Lock()
	defer cw.lock.Unlock()

	ctx := context.Background()
	var err error
	if conf.Common != nil {
		l.config.parseAndSetDynamicConfig(conf)

		v := l.config.ShouldSetMemoryReparentFile()

		go func() {
			cw.memReparentFileGoRoutineLock.Lock()
			defer cw.memReparentFileGoRoutineLock.Unlock()
			log.G(ctx).Errorf("dynamic config: setupAllCgroupsMemoryReparentFile to %v", v)
			err = setupAllCgroupsMemoryReparentFile(ctx, v)

			if err != nil {
				log.G(ctx).Errorf("failed to setupAllCgroupsMemoryReparentFile to %v: %v", v, err)
			}
		}()

	}

}

func setupAllCgroupsMemoryReparentFile(ctx context.Context, set bool) error {
	start := time.Now()

	defer func() {
		CubeLog.Infof("setupAllCgroupsMemoryReparentFile to %v took %dms", set, time.Since(start).Milliseconds())
	}()

	allCgIds, err := l.pool.GetAllGroupsExists()
	if err != nil {
		return fmt.Errorf("setupAllCgroupsMemoryReparentFile failed: %w", err)
	}

	errcnt := 0
	succCnt := 0
	for _, fullCgID := range allCgIds {
		err := setupMemoryReparentFileFn(ctx, fullCgID, set)
		if err != nil {
			errcnt++
			log.G(ctx).Errorf("failed to setup memory.reparent_file for cgroup %d: %v", fullCgID, err)
		} else {
			succCnt++
		}
	}

	if errcnt > 0 {
		return fmt.Errorf("failed to set all memory.reparent_file, succ:%d, fail:%d", succCnt, errcnt)
	} else {
		CubeLog.Infof("setupAllCgroupsMemoryReparentFile to %v success, succ:%d, fail:%d", set, succCnt, errcnt)
	}
	return nil
}

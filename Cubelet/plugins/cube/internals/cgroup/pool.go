// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"fmt"
	"math"
	"path"
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

	l.setupMemoryReparentFile(ctx, fullCgID, l.config.ShouldSetMemoryReparentFile())

	return nil
}

func (p *cgPool) init() error {

	allUse, err := p.db.ReadAll(bucket)
	if err != nil {
		return fmt.Errorf("recover cg data from db: %w", err)
	}

	p.mutex.Lock()
	p.dirtySet = make(map[uint32]struct{})
	p.individualGroups = make(map[string]struct{})
	err = p.initPoolV1()
	if err != nil {
		p.mutex.Unlock()
		return fmt.Errorf("init pool v1 failed: %w", err)
	}

	if err := p.initPoolV2(); err != nil {
		p.mutex.Unlock()
		return fmt.Errorf("init pool v2 failed: %w", err)
	}

	for _, inuseCgIdBytes := range allUse {
		inuseCgIdInt, err := strconv.Atoi(string(inuseCgIdBytes))
		if err != nil {
			continue
		}
		if inuseCgIdInt < CgPoolV1IdLimit {
			p.poolV1.initialAssign(uint32(inuseCgIdInt))
		} else {
			p.poolV2.initialAssign(uint32(inuseCgIdInt))
		}
	}
	poolV1ActualSize := p.poolV1.cgRanger.Cap()
	p.mutex.Unlock()

	err = p.initPoolV1Late(poolV1ActualSize)
	if err != nil {
		return fmt.Errorf("init pool v1 late failed: %w", err)
	}

	err = p.initPoolV2Late()
	if err != nil {
		return fmt.Errorf("init pool v2 late failed: %w", err)
	}
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
			l.setupMemoryReparentFile(ctx, uint32(cgId), l.config.ShouldSetMemoryReparentFile())
		}
	}

	return nil
}

func (p *cgPool) initPoolV2() error {
	numaNodes := numa.GetAllNumaNodes()
	maxNumaNodeID := 0
	for _, node := range numaNodes {
		if node.NodeId > maxNumaNodeID {
			maxNumaNodeID = node.NodeId
		}
	}
	nodeCount := maxNumaNodeID + 1
	p.poolV2.handle = p.poolV2Handle
	p.poolV2.numaPools = make([]cgPoolV2Numa, nodeCount)

	for _, nd := range numaNodes {
		numaId := nd.NodeId
		numaNode := &numaNodes[numaId]
		cgRanger, err := allocator.NewSimpleLinearRanger(0, uint16(p.initialSize))
		if err != nil {
			return fmt.Errorf("create cg ranger error: %w", err)
		}
		alloc := allocator.NewAllocator[uint16](cgRanger)
		p.poolV2.numaPools[numaId].allocator = alloc
		p.poolV2.numaPools[numaId].cgRanger = cgRanger
		p.poolV2.numaPools[numaId].numaNode = *numaNode
	}

	return nil
}

func (p *cgPool) initPoolV2Late() error {
	ctx := context.Background()
	for i := 0; i < len(p.poolV2.numaPools); i++ {
		numaPool := &p.poolV2.numaPools[i]
		numaNode := numaPool.numaNode
		log.G(ctx).Errorf("cgroup pool v2 init: nodeId: %d, cpus: %s", numaNode.NodeId, numaNode.Cpulist)
		err := p.poolV2.handle.CreateWithCpuSet(ctx, fmt.Sprintf("%s/numa%d", handle.DefaultPathPoolV2, numaNode.NodeId), numaNode.Cpulist, numaNode.NodeId)
		if err != nil {
			log.G(ctx).Errorf("cgroup pool v2 init, failed to create cgroup for numa%d: %v", numaNode.NodeId, err)
			return err
		}
		cap := uint16(numaPool.cgRanger.Cap())
		for shortCgId := uint16(0); shortCgId < cap; shortCgId++ {
			fullCgID := MakeFullCgID(shortCgId, true, int32(numaNode.NodeId))
			if err := p.createCgroup(ctx, fullCgID); err != nil {
				log.G(ctx).Errorf("cgroup pool v2 init: create fullCgID: %d failed: %v", fullCgID, err)
			} else {
				l.setupMemoryReparentFile(ctx, fullCgID, l.config.ShouldSetMemoryReparentFile())
			}

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

func (p *cgPool) Put(ctx context.Context, fullCgID uint32) {
	group := MakeCgroupPathByID(fullCgID)
	var err error
	if IsPoolV2ID(fullCgID) {
		err = p.poolV2Handle.CleanForReuse(ctx, group)
	} else {
		err = p.poolV1Handle.CleanForReuse(ctx, group)
	}
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if err == nil {
		p.allocatorReleaseCgroup(fullCgID)
		delete(p.dirtySet, fullCgID)
	} else {

		p.dirtySet[fullCgID] = struct{}{}
		log.G(ctx).Errorf("failed to clean cgroup %v for reuse: %w", MakeCgroupPathByID(fullCgID), err)
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

func (p *cgPool) tidyPoolV2(dirtyWork map[uint32]struct{}) error {
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
		err := l.setupMemoryReparentFile(ctx, fullCgID, set)
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

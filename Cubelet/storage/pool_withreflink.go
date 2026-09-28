// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"fmt"
	"hash/crc32"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func (l *local) initOtherFormatPool(dirtyList map[string]bool) error {

	q := resource.MustParse(defaultFormatSize)
	switch l.config.PoolType {
	case cp_type:
		otherFilePath := filepath.Join(l.otherFormatPath, baseFileName)
		if err := newExt4BaseRaw(otherFilePath, l.config.BaseDiskUUID, q.Value()); err != nil {
			return fmt.Errorf("init otherFilePath [%s]  failed, %s", otherFilePath, err.Error())
		}
		p := &pool{
			l:              l,
			format:         otherFormatSize,
			baseFormatPath: l.otherFormatPath,
			baseFormatFile: otherFilePath,
			pType:          l.config.PoolType,
		}
		l.poolFormat.Store(p.format, p)
	case cp_reflink_type:
		p := &poolWithReflink{
			l:                l,
			format:           otherFormatSize,
			formatSizeInByte: q.Value(),
			baseFormatPath:   l.otherFormatPath,
			pType:            l.config.PoolType,
			baseNum:          100,
		}
		l.poolFormat.Store(p.format, p)
		if err := p.init(dirtyList); err != nil {
			return fmt.Errorf("init format [%s]  failed, %s", q.String(), err.Error())
		}
	default:
		return fmt.Errorf("invalid pooltype %s", l.config.PoolType)
	}

	CubeLog.Infof("virtual disk_usage:%d", l.usedDiskSize.Load())
	return nil
}

type poolWithReflink struct {
	pType poolType
	sync.Once
	l                       *local
	format                  string
	formatSizeInByte        int64
	baseFormatPath          string
	poolWorkers             int
	triggerIntervalInSecond int
	triggerBurst            int
	devQueue                *utils.Queue[devInfo]
	cap                     int
	ingCount                int
	mutex                   sync.Mutex
	ch                      chan int
	exitCh                  chan struct{}
	exitWg                  sync.WaitGroup
	limiter                 *rate.Limiter
	baseNum                 uint64
	indexMap                map[uint64]*baseInfo
	FAdviseSize             int64

	prefetchBlocks []uint32
}

func (p *poolWithReflink) InitBaseFile(ctx context.Context) error {
	return nil
}
func (p *poolWithReflink) init(dirtyList map[string]bool) error {
	p.ch = make(chan int, p.poolWorkers)
	p.exitCh = make(chan struct{})
	p.devQueue = utils.NewQueue[devInfo]()
	p.ingCount = 0
	if p.triggerBurst != 0 {
		p.limiter = rate.NewLimiter(rate.Every(time.Duration(p.triggerIntervalInSecond)*time.Millisecond), p.triggerBurst)
	}
	p.indexMap = make(map[uint64]*baseInfo)
	for i := uint64(0); i < p.baseNum; i++ {
		bInfo := &baseInfo{
			refCnt:      0,
			maxRefNum:   30,
			FAdviseSize: p.FAdviseSize,
		}
		bInfo.baseFormatPath = filepath.Join(p.baseFormatPath, fmt.Sprintf("%d", i))
		if err := os.MkdirAll(bInfo.baseFormatPath, os.ModeDir|0755); err != nil {
			return fmt.Errorf("init otherFormatPath  [%s]] failed, %s", bInfo.baseFormatPath, err.Error())
		}
		bInfo.baseFormatFile = filepath.Join(bInfo.baseFormatPath, baseFileName)
		ok, _ := utils.DenExist(bInfo.baseFormatFile)
		if !ok {
			if err := newExt4BaseRaw(bInfo.baseFormatFile, p.l.config.BaseDiskUUID, p.formatSizeInByte); err != nil {
				return fmt.Errorf("init file [%s]  failed, %s", bInfo.baseFormatFile, err.Error())
			}
		}

		if p.prefetchBlocks == nil {
			gd, err := utils.GetExt4BlockGroupDescriptor(bInfo.baseFormatFile)
			if err != nil {
				return fmt.Errorf("get ext4 block group descriptor from %v failed: %w", bInfo.baseFormatFile, err)
			}
			p.prefetchBlocks = []uint32{0, 1, 2, gd.InodeTable}
			CubeLog.Infof("format: %v, prefetchBlocks: %v", p.format, p.prefetchBlocks)
		}

		bInfo.prefetchBlocks = p.prefetchBlocks
		p.indexMap[i] = bInfo
	}

	if localStorage.config != nil &&
		utils.Contains(p.format, localStorage.config.PoolDefaultFormatSizeList) {
		p.recover(dirtyList)
	}
	return nil
}

func (p *poolWithReflink) recover(dirty map[string]bool) {
	if len(dirty) == 0 {
		return
	}
	denList, err := ReadDir(p.baseFormatPath)
	if err != nil {
		return
	}
	q := resource.MustParse(p.format)
	p.mutex.Lock()
	defer p.mutex.Unlock()
	for _, den := range denList {
		if den.IsDir() {
			baseDirList, err := ReadDir(path.Clean(filepath.Join(p.baseFormatPath, den.Name())))
			if err != nil {
				continue
			}
			for _, bdir := range baseDirList {
				if !bdir.IsDir() {

					if bdir.Name() == baseFileName {
						continue
					}

					filePath := path.Join(path.Clean(filepath.Join(p.baseFormatPath, den.Name())), bdir.Name())

					if _, ok := dirty[filePath]; ok {
						continue
					}
					if p.devQueue.Length() < p.cap {
						fadvise(filePath, p.FAdviseSize, p.prefetchBlocks)
						p.devQueue.Enqueue(&devInfo{FilePath: filePath})
						p.l.incrSize(q.Value())
					} else {

						err = atomicDelete(filePath)
						CubeLog.Debugf("recover,clean over limit file:%s,err:%v", filePath, err)
					}
				}
			}
		}
	}
}

func (p *poolWithReflink) start() {
	workerNum := p.poolWorkers
	for i := 0; i < workerNum; i++ {
		recov.GoWithRecover(p.worker)
	}
	recov.GoWithRecover(p.daemonSupplementQueue)
}

func (p *poolWithReflink) allow() bool {
	if p.limiter == nil {
		return true
	}
	return p.limiter.Allow()
}

func (p *poolWithReflink) worker() {
	for {
		select {
		case <-p.exitCh:
			p.exitWg.Done()
			return
		case <-p.ch:
		}
		if p.allow() {
			recov.WithRecover(p.put)
		}
	}
}

func (p *poolWithReflink) daemonSupplementQueue() {
	for {
		select {
		case <-p.exitCh:
			return
		default:
		}
		time.Sleep(time.Duration(p.triggerIntervalInSecond) * time.Millisecond)
		quota := p.getQuota()
		for i := 0; i < quota; i++ {
			select {
			case p.ch <- 0:
			default:
			}
		}
	}
}

func (p *poolWithReflink) put() {
	var device *devInfo = nil
	if !p.expandStart() {
		return
	}

	id := uuid.New().String()
	real_index := uint64(crc32.ChecksumIEEE([]byte(id)))
	index := (real_index%p.baseNum + p.baseNum) % p.baseNum
	pIndex := p.indexMap[index]

	newFilePath := path.Join(pIndex.baseFormatPath, id)
	err := pIndex.New(newFilePath, 0)
	if err != nil {
		p.expandDone(device)
		CubeLog.Errorf("%s new devInfo error:%s", p.format, err)
		return
	}

	q := resource.MustParse(p.format)
	p.l.incrSize(q.Value())

	device = &devInfo{FilePath: newFilePath}
	p.expandDone(device)
}

func (p *poolWithReflink) Close() {
	p.Do(func() {
		if p.exitCh != nil {
			p.exitWg.Add(p.poolWorkers)
			close(p.exitCh)
			p.exitWg.Wait()
		}
	})
}

func (p *poolWithReflink) Get(ctx context.Context, size int64) (*devInfo, error) {
	device, err := p.getAsync(ctx)
	if err == nil {
		ok, err := utils.DenExist(device.FilePath)
		if ok {
			return device, nil
		} else {
			log.G(ctx).Errorf("%s in pool not exist,err:%v", device.FilePath, err)
		}
	}
	log.G(ctx).Warnf("%s pool has no more devs", p.format)
	return p.GetSync(ctx, size)
}

func (p *poolWithReflink) GetSync(ctx context.Context, size int64) (_ *devInfo, err error) {
	start := time.Now()
	defer func() {
		workflow.RecordCreateMetric(ctx, err, storageMetricNewFile, time.Since(start))
	}()

	id := uuid.New().String()
	real_index := uint64(crc32.ChecksumIEEE([]byte(id)))
	index := (real_index%p.baseNum + p.baseNum) % p.baseNum
	pIndex := p.indexMap[index]

	newFilePath := path.Join(pIndex.baseFormatPath, id)
	err = pIndex.New(newFilePath, size)
	if err != nil {
		return nil, err
	}

	p.l.incrSize(size)
	return &devInfo{FilePath: newFilePath}, nil
}

func (p *poolWithReflink) getAsync(ctx context.Context) (*devInfo, error) {
	defer func() {
		p.TriggerExpand()
	}()
	p.mutex.Lock()
	defer p.mutex.Unlock()
	dev := p.devQueue.Dequeue()
	if dev == nil {
		return nil, fmt.Errorf("no devInfo available in the pool")
	}
	return dev, nil
}

func (p *poolWithReflink) expandStart() bool {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if (p.devQueue.Length() + p.ingCount) >= p.cap {
		return false
	}
	p.ingCount++
	return true
}

func (p *poolWithReflink) expandDone(device *devInfo) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.ingCount--
	if device == nil {
		return
	}

	p.devQueue.Enqueue(device)
	p.TriggerExpand()
}

func (p *poolWithReflink) TriggerExpand() {
	select {
	case p.ch <- 0:
	default:
	}
}

func (p *poolWithReflink) getQuota() int {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	return p.cap - (p.devQueue.Length() + p.ingCount)
}

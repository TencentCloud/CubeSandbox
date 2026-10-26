// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"golang.org/x/sys/unix"
	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/api/resource"
)

type devInfo struct {
	FilePath string
}
type poolType string

const (
	cp_type         poolType = "copy"
	cp_reflink_type poolType = "copy_reflink"
)

type Pool interface {
	Get(ctx context.Context, size int64) (*devInfo, error)
	GetSync(ctx context.Context, size int64) (*devInfo, error)
	Close()
	InitBaseFile(ctx context.Context) error
}

type baseInfo struct {
	baseFormatPath string
	baseFormatFile string
	refCnt         int32
	maxRefNum      int32
	FAdviseSize    int64
	prefetchBlocks []uint32
}

func (b *baseInfo) New(newFilePath string, size int64) error {
	err := newExt4RawByReflinkCopy(b.baseFormatFile, newFilePath, size)
	if err != nil {
		return err
	}
	fadvise(newFilePath, b.FAdviseSize, b.prefetchBlocks)
	return nil
}

type pool struct {
	sync.Once
	l                       *local
	format                  string
	formatSizeInByte        int64
	baseFormatPath          string
	baseFormatFile          string
	poolWorkers             int
	triggerIntervalInSecond int
	triggerBurst            int
	devList                 []*devInfo
	size                    int
	cap                     int
	ingCount                int
	mutex                   sync.Mutex
	ch                      chan int
	exitCh                  chan struct{}
	pType                   poolType
	limiter                 *rate.Limiter
	FAdviseSize             int64

	prefetchBlocks []uint32
}

func (p *pool) InitBaseFile(ctx context.Context) error {
	return nil
}

func (p *pool) init(dirtyList map[string]bool) error {
	p.ch = make(chan int, p.poolWorkers)
	p.exitCh = make(chan struct{})
	p.devList = make([]*devInfo, p.cap)
	p.size = 0
	p.ingCount = 0
	if p.triggerBurst != 0 {
		p.limiter = rate.NewLimiter(rate.Every(time.Duration(p.triggerIntervalInSecond)*time.Millisecond), p.triggerBurst)
	}
	p.baseFormatFile = filepath.Join(p.baseFormatPath, baseFileName)
	if err := newExt4BaseRaw(p.baseFormatFile, p.l.config.BaseDiskUUID, p.formatSizeInByte); err != nil {
		return fmt.Errorf("init file [%s]  failed, %s", p.baseFormatFile, err.Error())
	}

	gd, err := utils.GetExt4BlockGroupDescriptor(p.baseFormatFile)
	if err != nil {
		return fmt.Errorf("get ext4 block group descriptor from %v failed: %w", p.baseFormatFile, err)
	}
	p.prefetchBlocks = []uint32{0, 1, 2, gd.InodeTable}
	CubeLog.Infof("format %v: prefetchBlocks: %v", p.format, p.prefetchBlocks)

	p.recover(dirtyList)
	return nil
}

func (p *pool) recover(dirty map[string]bool) {
	denList, err := ReadDir(p.baseFormatPath)
	if err != nil {
		return
	}
	q := resource.MustParse(p.format)
	p.mutex.Lock()
	defer p.mutex.Unlock()
	for _, den := range denList {
		if den.IsDir() {
			continue
		}

		if den.Name() == baseFileName {
			continue
		}

		filePath := path.Join(p.baseFormatPath, den.Name())

		if _, ok := dirty[filePath]; ok {
			continue
		}
		if p.size < p.cap {
			fadvise(filePath, p.FAdviseSize, p.prefetchBlocks)
			p.devList[p.size] = &devInfo{FilePath: filePath}
			p.size++
			p.l.incrSize(q.Value())
		} else {

			err = atomicDelete(filePath)
			CubeLog.Debugf("recover,clean over limit file:%s,err:%v", filePath, err)
		}
	}
}

func (p *pool) start() {
	workerNum := p.poolWorkers
	for i := 0; i < workerNum; i++ {
		recov.GoWithRecover(p.worker)
	}
	recov.GoWithRecover(p.daemonSupplementQueue)
}

func (p *pool) allow() bool {
	if p.limiter == nil {
		return true
	}
	return p.limiter.Allow()
}

func (p *pool) worker() {
	for range p.ch {
		select {
		case <-p.exitCh:
			return
		default:
		}
		if p.allow() {
			recov.WithRecover(p.put)
		}
	}
}

func (p *pool) daemonSupplementQueue() {
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

func (p *pool) put() {
	var device *devInfo = nil
	if !p.expandStart() {
		return
	}

	newFilePath := path.Join(p.baseFormatPath, uuid.New().String())
	err := newExt4RawByCopy(p.baseFormatFile, newFilePath, 0)
	if err != nil {
		p.expandDone(device)
		CubeLog.Errorf("%s new devInfo error:%s", p.format, err)
		return
	}
	fadvise(newFilePath, p.FAdviseSize, p.prefetchBlocks)
	device = &devInfo{FilePath: newFilePath}
	p.expandDone(device)
}

func (p *pool) Close() {
	p.Do(func() {
		if p.exitCh != nil {
			close(p.exitCh)
		}
	})
}

func (p *pool) Get(ctx context.Context, size int64) (*devInfo, error) {
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

func (p *pool) GetSync(ctx context.Context, size int64) (_ *devInfo, err error) {
	start := time.Now()
	defer func() {
		workflow.RecordCreateMetric(ctx, err, storageMetricNewFile, time.Since(start))
	}()
	newFilePath := path.Join(p.baseFormatPath, uuid.New().String())
	err = newExt4RawByCopy(p.baseFormatFile, newFilePath, size)
	if err != nil {
		return nil, err
	}

	fadvise(newFilePath, p.FAdviseSize, p.prefetchBlocks)

	p.l.incrSize(size)
	return &devInfo{FilePath: newFilePath}, nil
}

func (p *pool) getAsync(ctx context.Context) (*devInfo, error) {
	defer func() {
		p.TriggerExpand()
	}()
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if p.size == 0 {
		return nil, fmt.Errorf("no devInfo available in the pool")
	}

	p.size--
	return p.devList[p.size], nil
}

func (p *pool) expandStart() bool {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if (p.size + p.ingCount) >= p.cap {
		return false
	}
	p.ingCount++
	return true
}

func (p *pool) expandDone(device *devInfo) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.ingCount--
	if device == nil {
		return
	}

	p.devList[p.size] = device
	p.size++
	p.TriggerExpand()
}

func (p *pool) TriggerExpand() {
	select {
	case p.ch <- 0:
	default:
	}
}

func (p *pool) getQuota() int {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	return p.cap - (p.size + p.ingCount)
}

func fadvise(filePath string, size int64, blocks []uint32) {
	file, err := os.OpenFile(filePath, os.O_RDWR, 0755)
	if err != nil {
		return
	}
	defer file.Close()

	b := make([]byte, 1)
	n, _ := file.ReadAt(b, 0)
	if n == 1 {
		file.WriteAt(b, 0)
	}

	if size > 0 {
		_ = unix.Fadvise(int(file.Fd()), 0, size, unix.FADV_WILLNEED)
	}

	for _, offset := range blocks {
		_ = unix.Fadvise(int(file.Fd()), int64(offset)*4096, 4096, unix.FADV_WILLNEED)
	}
}

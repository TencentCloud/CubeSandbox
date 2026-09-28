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
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

const (
	defaultBaseNum = 3

	defaultMaxRefNum = 30
)

type cleanupCtx struct{}

func (l *local) initCubeboxFormatPool() error {

	denList, err := ReadDir(l.cubeboxTemplateFormatPath)
	if err != nil {
		return err
	}
	for _, den := range denList {
		if den.IsDir() {
			templateID := den.Name()
			if templateID == "." || templateID == ".." {
				continue
			}
			baseFormatDir := filepath.Join(l.cubeboxTemplateFormatPath, templateID)
			format := func() string {
				data, err := os.ReadFile(filepath.Join(baseFormatDir, formatFileName))
				if err != nil {
					return ""
				}
				return string(data)
			}()
			if err := l.initTmpCubeboxFormatPool(templateID, format); err != nil {
				CubeLog.Errorf("initTmpCubeboxFormatPool failed:%v", templateID, err)
			}
			switch l.config.PoolType {
			case cp_type:
				p := &pool{
					l:              l,
					format:         format,
					baseFormatPath: baseFormatDir,
					baseFormatFile: filepath.Join(baseFormatDir, baseFileName),
					pType:          l.config.PoolType,
				}
				l.poolFormat.Store(templateID, p)
			case cp_reflink_type:

				targetDir := baseFormatDir
				currentLink := filepath.Join(baseFormatDir, "current")
				if linkDest, err := os.Readlink(currentLink); err == nil {

					targetDir = filepath.Join(baseFormatDir, linkDest)
				}

				baseDirList, err := ReadDir(path.Clean(targetDir))
				if err != nil {
					CubeLog.Errorf("initCubeboxFormatPool:%s ReadDir failed:%v", templateID, err)
					continue
				}
				baseNum := uint64(0)
				for _, bdir := range baseDirList {

					if bdir.IsDir() && utils.IsInteger(bdir.Name()) {

						baseNum++
					}
				}
				if baseNum == 0 {
					CubeLog.Fatalf("initCubeboxFormatPool:%s has no subdir", templateID)
					continue
				}

				p := &cubeboxWithReflink{
					l:              l,
					format:         format,
					baseFormatPath: baseFormatDir,
					pType:          l.config.PoolType,
					baseNum:        baseNum,
				}

				if p.format != "" {
					q := resource.MustParse(p.format)
					p.formatSizeInByte = q.Value()
				}
				if err := p.initMultiBaseFile(context.Background()); err != nil {
					CubeLog.Fatalf("initCubeboxFormatPool:%s initMultiBaseFile failed:%v", templateID, err)
					continue
				}
				l.poolFormat.Store(templateID, p)
			}
		}
	}
	return nil
}

func (l *local) initTmpCubeboxFormatPool(templateID string, format string) error {
	baseFormatDir := filepath.Join(l.cubeboxTemplateFormatPath, templateID)
	baseFilePath := filepath.Join(baseFormatDir, baseFileName)
	exist, err := utils.FileExistAndValid(baseFilePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	if err == nil && exist {
		switch l.config.PoolType {
		case cp_type:
			p := &pool{
				l:              l,
				format:         format,
				baseFormatPath: baseFormatDir,
				baseFormatFile: baseFilePath,
				pType:          l.config.PoolType,
			}
			l.tmpPoolFormat.Store(templateID, p)
		case cp_reflink_type:
			p := &cubeboxWithReflink{
				l:              l,
				format:         format,
				baseFormatPath: baseFormatDir,
				pType:          l.config.PoolType,
				baseNum:        defaultBaseNum,
			}
			if p.format != "" {
				q := resource.MustParse(p.format)
				p.formatSizeInByte = q.Value()
			}

			l.tmpPoolFormat.Store(templateID, p)
		default:
			return fmt.Errorf("invalid pooltype %s", l.config.PoolType)
		}
	}
	return nil
}

func (l *local) newCubeboxFormatPool(ctx context.Context, templateID string, size string) (string, error) {

	baseFormatDir := filepath.Join(l.cubeboxTemplateFormatPath, templateID)
	if err := os.MkdirAll(path.Clean(baseFormatDir), os.ModeDir|0755); err != nil {
		return "", fmt.Errorf("%v  MkdirAll failed:%w", baseFormatDir, err)
	}

	q := resource.MustParse(size)
	if err := os.WriteFile(filepath.Join(baseFormatDir, formatFileName), []byte(size), 0644); err != nil {
		return "", fmt.Errorf("write format file failed: %w", err)
	}

	baseFilePath := filepath.Join(baseFormatDir, baseFileName)
	log.G(ctx).Infof("newCubeboxFormatPool:%s", baseFilePath)
	if err := newExt4BaseRawWithReplace(baseFilePath, l.config.BaseDiskUUID, q.Value(), true); err != nil {
		return "", fmt.Errorf("init baseFilePath [%s]  failed, %w", baseFilePath, err)
	}
	switch l.config.PoolType {
	case cp_type:
		p := &pool{
			l:              l,
			format:         size,
			baseFormatPath: baseFormatDir,
			baseFormatFile: baseFilePath,
			pType:          l.config.PoolType,
		}
		l.tmpPoolFormat.Store(templateID, p)
	case cp_reflink_type:
		p := &cubeboxWithReflink{
			l:                l,
			format:           size,
			formatSizeInByte: q.Value(),
			baseFormatPath:   baseFormatDir,
			pType:            l.config.PoolType,
			baseNum:          defaultBaseNum,
		}

		l.tmpPoolFormat.Store(templateID, p)
	default:
		return "", fmt.Errorf("invalid pooltype %s", l.config.PoolType)
	}

	return baseFilePath, nil
}

type cubeboxWithReflink struct {
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

func (p *cubeboxWithReflink) InitBaseFile(ctx context.Context) error {
	baseFilePath := filepath.Join(p.baseFormatPath, baseFileName)
	log.G(ctx).Infof("InitBaseFile:%s", baseFilePath)
	if _, err := os.Stat(baseFilePath); os.IsNotExist(err) {
		return fmt.Errorf("baseFilePath %s not exist", baseFilePath)
	}

	oldVersion := ""
	currentLink := filepath.Join(p.baseFormatPath, "current")
	if target, err := os.Readlink(currentLink); err == nil {
		oldVersion = target
	}

	if err := p.ensureLegacyCompatibility(ctx); err != nil {
		return err
	}

	version := fmt.Sprintf("ver_%d", time.Now().UnixNano())

	if err := p.updateBaseFile(ctx, version); err != nil {
		return err
	}

	go func() {

		if oldVersion != "" {

			oldPath := filepath.Join(p.baseFormatPath, oldVersion)
			if err := os.RemoveAll(oldPath); err != nil {
				log.G(ctx).Warnf("failed to remove old version %s: %v", oldPath, err)
			} else {
				log.G(ctx).Infof("removed old version %s", oldPath)
			}
		}

		if config.GetCommon().DisableCubeBoxTemplateBaseFormatPoolOfNumberVer {

			entries, err := os.ReadDir(p.baseFormatPath)
			if err != nil {
				log.G(ctx).Warnf("failed to read dir %s for cleanup: %v", p.baseFormatPath, err)
				return
			}
			for _, entry := range entries {
				if entry.IsDir() && utils.IsInteger(entry.Name()) {
					pathToRemove := filepath.Join(p.baseFormatPath, entry.Name())
					if err := os.RemoveAll(pathToRemove); err != nil {
						log.G(ctx).Warnf("failed to remove old base dir %s: %v", pathToRemove, err)
					} else {
						log.G(ctx).Infof("removed old base dir %s", pathToRemove)
					}
				}
			}

		}
	}()

	return nil
}

func (p *cubeboxWithReflink) ensureLegacyCompatibility(ctx context.Context) error {
	if config.GetCommon().DisableCubeBoxTemplateBaseFormatPoolOfNumberVer {
		return nil
	}
	log.G(ctx).Infof("ensureLegacyCompatibility: generating/updating legacy directories for %s", p.baseFormatPath)

	_, err := p.prepareNewVersion(ctx, "")
	return err
}

func (p *cubeboxWithReflink) updateBaseFile(ctx context.Context, version string) error {

	newIndexMap, err := p.prepareNewVersion(ctx, version)
	if err != nil {
		return err
	}

	if version != "" {

		currentLink := filepath.Join(p.baseFormatPath, "current")

		tmpLink := filepath.Join(p.baseFormatPath, "current_tmp")
		_ = os.Remove(tmpLink)
		if err := os.Symlink(version, tmpLink); err != nil {
			return fmt.Errorf("failed to create symlink: %w", err)
		}

		if err := os.Rename(tmpLink, currentLink); err != nil {
			return fmt.Errorf("failed to update current symlink: %w", err)
		}
	}
	p.indexMap = newIndexMap
	log.G(ctx).Infof("cubebox template %s switched to version %s", filepath.Base(p.baseFormatPath), version)
	return nil
}

func (p *cubeboxWithReflink) prepareNewVersion(ctx context.Context, version string) (map[uint64]*baseInfo, error) {
	baseFilePath := filepath.Join(p.baseFormatPath, baseFileName)

	targetBasePath := p.baseFormatPath
	if version != "" {
		targetBasePath = filepath.Join(p.baseFormatPath, version)
		if err := os.Mkdir(targetBasePath, os.ModeDir|0755); err != nil && !os.IsExist(err) {
			return nil, fmt.Errorf("prepareNewVersion mkdir [%s] failed: %w", targetBasePath, err)
		}
	}

	newIndexMap := make(map[uint64]*baseInfo)
	for i := uint64(0); i < p.baseNum; i++ {
		bInfo := &baseInfo{
			refCnt:      0,
			maxRefNum:   defaultMaxRefNum,
			FAdviseSize: p.FAdviseSize,
		}
		bInfo.baseFormatPath = filepath.Join(targetBasePath, strconv.FormatUint(i, 10))
		if err := os.Mkdir(bInfo.baseFormatPath, os.ModeDir|0755); err != nil && !os.IsExist(err) {
			return nil, fmt.Errorf("prepareNewVersion mkdir [%s] failed: %w", bInfo.baseFormatPath, err)
		}
		bInfo.baseFormatFile = filepath.Join(bInfo.baseFormatPath, baseFileName)
		if err := utils.SafeCopyFile(bInfo.baseFormatFile, baseFilePath); err != nil {
			return nil, fmt.Errorf("prepareNewVersion safe copy failed: %w", err)
		}

		ok, err := utils.FileExistAndValid(bInfo.baseFormatFile)
		if !ok {
			return nil, fmt.Errorf("prepareNewVersion file (%s) not exist or invalid: %v", bInfo.baseFormatFile, err)
		}

		if p.prefetchBlocks == nil {
			gd, err := utils.GetExt4BlockGroupDescriptor(bInfo.baseFormatFile)
			if err != nil {
				return nil, fmt.Errorf("get ext4 block group descriptor from %v failed: %w", bInfo.baseFormatFile, err)
			}
			p.prefetchBlocks = []uint32{0, 1, 2, gd.InodeTable}
			log.G(ctx).Infof("format: %v, prefetchBlocks: %v", p.format, p.prefetchBlocks)
		}

		bInfo.prefetchBlocks = p.prefetchBlocks
		newIndexMap[i] = bInfo
	}
	return newIndexMap, nil
}

func (p *cubeboxWithReflink) initMultiBaseFile(ctx context.Context) error {

	currentLink := filepath.Join(p.baseFormatPath, "current")
	version := ""
	if target, err := os.Readlink(currentLink); err == nil {
		version = target
	}

	newIndexMap, err := p.loadExistingVersion(ctx, version)
	if err != nil {
		return err
	}

	p.indexMap = newIndexMap

	p.ch = make(chan int, p.poolWorkers)
	p.exitCh = make(chan struct{})
	p.devQueue = utils.NewQueue[devInfo]()
	p.ingCount = 0
	if p.triggerBurst != 0 {
		p.limiter = rate.NewLimiter(rate.Every(time.Duration(p.triggerIntervalInSecond)*time.Millisecond), p.triggerBurst)
	}

	return nil
}

func (p *cubeboxWithReflink) loadExistingVersion(ctx context.Context, version string) (map[uint64]*baseInfo, error) {
	targetBasePath := p.baseFormatPath
	if version != "" {
		targetBasePath = filepath.Join(p.baseFormatPath, version)
	}

	newIndexMap := make(map[uint64]*baseInfo)
	found := 0
	for i := uint64(0); i < p.baseNum; i++ {
		bInfo := &baseInfo{
			refCnt:      0,
			maxRefNum:   defaultMaxRefNum,
			FAdviseSize: p.FAdviseSize,
		}
		bInfo.baseFormatPath = filepath.Join(targetBasePath, strconv.FormatUint(i, 10))

		bInfo.baseFormatFile = filepath.Join(bInfo.baseFormatPath, baseFileName)

		if ok, _ := utils.FileExistAndValid(bInfo.baseFormatFile); ok {

			if p.prefetchBlocks == nil {
				gd, err := utils.GetExt4BlockGroupDescriptor(bInfo.baseFormatFile)
				if err != nil {
					log.G(ctx).Warnf("get ext4 block group descriptor from %v failed: %v", bInfo.baseFormatFile, err)
				} else {
					p.prefetchBlocks = []uint32{0, 1, 2, gd.InodeTable}
				}
			}
			bInfo.prefetchBlocks = p.prefetchBlocks
			newIndexMap[i] = bInfo
			found++
		}
	}

	if found == 0 {
		return nil, fmt.Errorf("no valid base files found in %s", targetBasePath)
	}
	return newIndexMap, nil
}

func (p *cubeboxWithReflink) start() {
	workerNum := p.poolWorkers
	for i := 0; i < workerNum; i++ {
		recov.GoWithRecover(p.worker)
	}
	recov.GoWithRecover(p.daemonSupplementQueue)
}

func (p *cubeboxWithReflink) allow() bool {
	if p.limiter == nil {
		return true
	}
	return p.limiter.Allow()
}

func (p *cubeboxWithReflink) worker() {
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

func (p *cubeboxWithReflink) daemonSupplementQueue() {
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

func (p *cubeboxWithReflink) put() {
	if p.indexMap == nil {
		return
	}

	var device *devInfo = nil
	if !p.expandStart() {
		return
	}

	id := uuid.New().String()
	realIndex := uint64(crc32.ChecksumIEEE([]byte(id)))
	index := (realIndex%p.baseNum + p.baseNum) % p.baseNum
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

func (p *cubeboxWithReflink) Close() {
	p.Do(func() {
		if p.exitCh != nil {
			p.exitWg.Add(p.poolWorkers)
			close(p.exitCh)
			p.exitWg.Wait()
		}
	})
}

func (p *cubeboxWithReflink) Get(ctx context.Context, size int64) (*devInfo, error) {
	return p.GetSync(ctx, size)
}

func (p *cubeboxWithReflink) GetSync(ctx context.Context, size int64) (_ *devInfo, err error) {
	if p.indexMap == nil {
		return nil, fmt.Errorf("cubeboxWithReflink should be init first")
	}
	start := time.Now()
	defer func() {
		workflow.RecordCreateMetric(ctx, err, storageMetricNewFile, time.Since(start))
	}()

	id := uuid.New().String()
	realIndex := uint64(crc32.ChecksumIEEE([]byte(id)))
	index := (realIndex%p.baseNum + p.baseNum) % p.baseNum
	pIndex := p.indexMap[index]

	newFilePath := path.Join(pIndex.baseFormatPath, id)
	err = pIndex.New(newFilePath, size)
	if err != nil {
		return nil, err
	}

	p.l.incrSize(size)
	return &devInfo{FilePath: newFilePath}, nil
}

func (p *cubeboxWithReflink) getAsync(ctx context.Context) (*devInfo, error) {
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

func (p *cubeboxWithReflink) expandStart() bool {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if (p.devQueue.Length() + p.ingCount) >= p.cap {
		return false
	}
	p.ingCount++
	return true
}

func (p *cubeboxWithReflink) expandDone(device *devInfo) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.ingCount--
	if device == nil {
		return
	}

	p.devQueue.Enqueue(device)
	p.TriggerExpand()
}

func (p *cubeboxWithReflink) TriggerExpand() {
	select {
	case p.ch <- 0:
	default:
	}
}

func (p *cubeboxWithReflink) getQuota() int {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	return p.cap - (p.devQueue.Length() + p.ingCount)
}

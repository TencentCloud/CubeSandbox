// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/tomlext"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cdp"
	dynamConf "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/multilock"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/multimeta"

	"encoding/json"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/plugin"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/multimetadb/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/disk"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type local struct {
	config *Config

	poolFormat sync.Map

	tmpPoolFormat sync.Map

	otherFormatPath           string
	cubeboxTemplateFormatPath string
	diskSize                  int64
	diskWarningSize           int64
	usedDiskSize              atomic.Int64

	capBlocks, capInodes atomic.Int32
	db                   *utils.CubeStore
	hostInfo             *HostStorageMeta
	cubeboxAPI           cubes.CubeboxAPI
	multiLock            *multilock.MultiLock
}

type HostStorageMeta struct {
	InstanceID string
	IP         string
}

var localStorage = &local{}

var (
	defaultPoolSize                = 500
	defaultPoolWorkers             = 8
	defaultPoolTriggerIntervalInMs = 1000
	defaultWarningPercent          = 100
	defaultFormatSize              = "1Gi"

	unifiedStorageSize        = resource.MustParse(defaultFormatSize)
	otherFormatSize           = "othersv2"
	cubeboxTemplateFormatSize = "cubebox"
	defaultDiskUUID           = "ef5c2893-ddbd-4d6e-bef6-3853c31d5b94"
	emptyDir                  = "emptydir"
	failoverDir               = "failoverdir"
	dbDir                     = "db"

	bucketName                 = "emptydir/v1"
	stubKeyName                = "cube_stub"
	emptyDirInnerSourcePath    = "disk"
	defaultFreeBlocksThreshold = int32(15)
	defaultFreeInodesThreshold = int32(15)

	dbBucketList = []*multimeta.BucketDefineInternal{
		{
			BucketDefine: &multimetadb.BucketDefine{
				Name:     bucketName,
				DbName:   "storage",
				Describe: "storage plugin db",
			},
		},
		{
			BucketDefine: &multimetadb.BucketDefine{
				Name:     nfsBucketName,
				DbName:   "storage",
				Describe: "storage plugin db",
			},
		},
	}
	nfsBucketName  = "nfs/v1"
	failoverNfsDir = "nfsfailoverdir"
	randomSeed     = rand.New(rand.NewSource(time.Now().UnixNano()))
)

const (
	baseFileName         = "base.raw"
	formatFileName       = "format"
	storageMetricDB      = "cube-storage-db"
	storageMetricNewFile = "cube-storage-new"
)

func (l *local) init(ic *plugin.InitContext) error {
	l.multiLock = multilock.NewMultiLock(multilock.NewMultiLockOptions())
	if l.config.PoolSize <= 0 {
		l.config.PoolSize = defaultPoolSize
	}
	if l.config.PoolWorkers <= 0 {
		l.config.PoolWorkers = defaultPoolWorkers
	}
	if l.config.PoolTriggerIntervalInMs <= 0 {
		l.config.PoolTriggerIntervalInMs = defaultPoolTriggerIntervalInMs
	}
	if l.config.WarningPercent <= 0 {
		l.config.WarningPercent = int64(defaultWarningPercent)
	}
	if l.config.BaseDiskUUID == "" {
		l.config.BaseDiskUUID = defaultDiskUUID
	}

	if l.config.FAdviseSize <= 0 {
		l.config.FAdviseSize = 256 * 1024
	}

	if l.config.FreeBlocksThreshold <= 0 {
		l.config.FreeBlocksThreshold = defaultFreeBlocksThreshold
	}

	if l.config.FreeInodesThreshold <= 0 {
		l.config.FreeInodesThreshold = defaultFreeInodesThreshold
	}
	q, err := resource.ParseQuantity(l.config.DiskSize)
	if err != nil {
		CubeLog.Errorf("invalid DiskSize :%s", l.config.DiskSize)
		return err
	}
	l.diskSize = q.Value()
	l.diskWarningSize = l.diskSize * l.config.WarningPercent / 100

	if len(l.config.PoolDefaultFormatSizeList) == 0 {
		l.config.PoolDefaultFormatSizeList = append(l.config.PoolDefaultFormatSizeList, defaultFormatSize)
	}
	for _, d := range []string{l.config.RootPath, l.config.DataPath} {
		if err := os.MkdirAll(path.Clean(d), os.ModeDir|0755); err != nil {
			return fmt.Errorf("%v  MkdirAll failed:%v", d, err.Error())
		}
	}

	if err := l.initDb(); err != nil {
		return err
	}
	if err := l.initEmptyDir(); err != nil {
		return err
	}

	if err := l.initHostInfo(); err != nil {
		return err
	}

	if l.config.ReconcileInterval == 0 {
		l.config.ReconcileInterval = tomlext.FromStdTime(5 * time.Minute)
	}
	go l.loopReconcile(ic.Context)
	go l.loopUpdateStatus(ic.Context)
	return nil
}

func (l *local) loopUpdateStatus(context context.Context) {
	gcTicker := time.NewTicker(tomlext.ToStdTime(l.config.ReconcileInterval))
	defer gcTicker.Stop()

	for {
		select {
		case <-gcTicker.C:
			l.updateBlocksCap()
		case <-context.Done():
			return
		}
	}
}

func (l *local) Close() error {
	return nil
}

func (l *local) initHostInfo() error {
	identity, err := utils.GetHostIdentity()
	if err != nil {
		return fmt.Errorf("get host identity failed: %w", err)
	}
	l.hostInfo = &HostStorageMeta{
		InstanceID: identity.InstanceID,
		IP:         identity.LocalIPv4,
	}

	return nil
}

func (l *local) initEmptyDir() error {
	basePath := filepath.Join(l.config.DataPath, emptyDir)
	l.otherFormatPath = filepath.Join(basePath, otherFormatSize)
	failoverPath := filepath.Join(l.config.DataPath, failoverDir)
	l.cubeboxTemplateFormatPath = filepath.Join(basePath, cubeboxTemplateFormatSize)
	for _, d := range []string{basePath, failoverPath, l.otherFormatPath, l.cubeboxTemplateFormatPath} {
		if err := os.MkdirAll(path.Clean(d), os.ModeDir|0755); err != nil {
			return fmt.Errorf("%v  MkdirAll failed:%v", d, err.Error())
		}
	}

	dirtyList := l.listDirtyStorage()

	if err := l.initFormatPool(basePath, dirtyList); err != nil {
		return err
	}

	if err := l.initOtherFormatPool(dirtyList); err != nil {
		return err
	}

	if err := l.initCubeboxFormatPool(); err != nil {
		return err
	}
	return nil
}

func (l *local) initFormatPool(basePath string, dirtyList map[string]bool) error {

	for _, s := range l.config.PoolDefaultFormatSizeList {
		q, err := resource.ParseQuantity(s)
		if err != nil {
			CubeLog.Errorf("PoolDefaultFormatSizeList :%s parse fail", s)
			continue
		}
		baseFormatPath := filepath.Join(basePath, q.String())
		if err := os.MkdirAll(baseFormatPath, os.ModeDir|0755); err != nil {
			return fmt.Errorf("init dir  [%s]] failed, %s", baseFormatPath, err.Error())
		}

		switch l.config.PoolType {
		case cp_type:
			p := &pool{
				l:                       l,
				format:                  q.String(),
				formatSizeInByte:        q.Value(),
				baseFormatPath:          baseFormatPath,
				cap:                     dynamConf.GetPoolSizeForInit(l.config.PoolSize),
				poolWorkers:             l.config.PoolWorkers,
				triggerIntervalInSecond: l.config.PoolTriggerIntervalInMs,
				triggerBurst:            l.config.PoolTriggerBurst,
				pType:                   l.config.PoolType,
			}
			l.poolFormat.Store(p.format, p)
			if err := p.init(dirtyList); err != nil {
				return fmt.Errorf("init format [%s]  failed, %s", q.String(), err.Error())
			}
			p.start()
		case cp_reflink_type:
			p := &poolWithReflink{
				l:                       l,
				format:                  q.String(),
				formatSizeInByte:        q.Value(),
				baseFormatPath:          baseFormatPath,
				pType:                   l.config.PoolType,
				baseNum:                 100,
				cap:                     dynamConf.GetPoolSizeForInit(l.config.PoolSize),
				poolWorkers:             l.config.PoolWorkers,
				triggerIntervalInSecond: l.config.PoolTriggerIntervalInMs,
				triggerBurst:            l.config.PoolTriggerBurst,
			}
			l.poolFormat.Store(p.format, p)
			if err := p.init(dirtyList); err != nil {
				return fmt.Errorf("init format [%s]  failed, %s", q.String(), err.Error())
			}
			p.start()
		default:
			return fmt.Errorf("invalid pooltype %s", l.config.PoolType)
		}
	}
	return nil
}

func (l *local) listDirtyStorage() map[string]bool {

	all, err := l.readAllFileInfo()
	if err != nil {
		return map[string]bool{}
	}
	ctx := context.WithValue(context.Background(), CubeLog.KeyCaller, constants.StorageID.ID())
	ctx = context.WithValue(ctx, CubeLog.KeyCallee, constants.StorageID.ID())
	dirtyList := make(map[string]bool)
	for id, data := range all {
		logEntry := log.G(ctx).WithFields(CubeLog.Fields{
			string(CubeLog.KeyInstanceId): id,
			"step":                        "storage listDirtyStorage",
			"info":                        string(data),
		})
		if id == stubKeyName {
			continue
		}
		bf := &StorageInfo{}
		err = json.Unmarshal(data, bf)
		if err != nil {
			logEntry.Errorf("storage data leak: failed to unmarshal StorageInfo: %v", err)
			continue
		}

		if bf.InstanceType == cubebox.InstanceType_cubebox.String() && bf.TemplateID != "" {
			if !bf.UpdateAt.IsZero() && time.Since(bf.UpdateAt) > 7*24*time.Hour {
				logEntry.Infof("storage data leak: cubebox template %s is outdated, last update at: %v", bf.TemplateID, bf.UpdateAt)
			}
			continue
		}

		if id != bf.SandboxID {
			logEntry.Warnf("storage data leak: id is not equeal to sandbox id:%s,%s", id, bf.SandboxID)
			continue
		}

		cb, err := l.cubeboxAPI.Get(ctx, bf.SandboxID)
		taskExist := err == nil && cb != nil
		if taskExist {
			for _, v := range bf.Volumes {
				if v.Medium == cubebox.StorageMedium_StorageMediumDefault {
					if fileExist, _ := utils.DenExist(v.FilePath); fileExist {
						dirtyList[v.FilePath] = true

						l.incrSize(v.SizeLimit)
					} else {

						logEntry.Fatalf("listDirtyStorage,task %s alive,"+
							" but storage file not exist:%s", bf.SandboxID, v.FilePath)
					}
				}
			}
		} else {
			for range bf.Volumes {
				logEntry.Errorf("storage data leak: local storage may leaked")
			}
			logEntry.Errorf("storage data leak: backend file info for %s may leaked", bf.SandboxID)
		}
	}
	return dirtyList
}

func (l *local) initDb() error {
	basePath := filepath.Join(l.config.RootPath, dbDir)
	if err := os.MkdirAll(path.Clean(basePath), os.ModeDir|0755); err != nil {
		return fmt.Errorf("init dir failed %s", err.Error())
	}
	var err error
	if l.db, err = utils.NewCubeStoreExt(basePath, "meta.db", 10, nil); err != nil {
		return err
	}

	err = l.db.Set(bucketName, stubKeyName, []byte{})
	if err != nil {
		return err
	}

	for _, bucket := range dbBucketList {
		bucket.CubeStore = l.db
		multimeta.RegisterBucket(bucket)
	}
	return nil
}

func (l *local) ID() string {
	return constants.StorageID.ID()
}

func (l *local) Init(ctx context.Context, opts *workflow.InitInfo) error {
	log.G(ctx).Errorf("Init doing")

	defer log.G(ctx).Errorf("Init end")

	all, err := l.readAllFileInfo()

	if err == nil {
		for k, v := range all {
			if k == stubKeyName {
				continue
			}
			bf := &StorageInfo{}
			err = json.Unmarshal(v, bf)
			if err != nil {
				log.G(ctx).Errorf("Init:load Metadata,unmarshal fail:%v", err)
				continue
			}
			if bf.InstanceType == cubebox.InstanceType_cubebox.String() &&
				bf.TemplateID != "" {
				continue
			}
			err = l.destroy(ctx, bf, nil)
			if err != nil {
				log.G(ctx).Errorf("Init:destroy fail:%v", err)
			}
		}
	}

	l.poolFormat.Range(func(key, value interface{}) bool {
		v := value.(Pool)
		v.Close()
		return true
	})

	_ = l.db.Close()

	time.Sleep(time.Second)
	if err := os.RemoveAll(path.Clean(l.config.RootPath)); err != nil {
		return fmt.Errorf("%v  RemoveAll failed:%v", l.config.RootPath, err.Error())
	}
	if err := os.MkdirAll(filepath.Join(l.config.RootPath, failoverDir), os.ModeDir|0755); err != nil {
		return fmt.Errorf("init dir failed %s", err.Error())
	}

	if err := os.MkdirAll(path.Clean(l.config.RootPath), os.ModeDir|0755); err != nil {
		return fmt.Errorf("%v MkdirAll RootPath failed: %s", l.config.RootPath, err.Error())
	}
	if err := os.RemoveAll(path.Clean(l.config.DataPath)); err != nil {
		return fmt.Errorf("%v  RemoveAll failed:%v", l.config.DataPath, err.Error())
	}

	if err := os.MkdirAll(path.Clean(l.config.DataPath), os.ModeDir|0755); err != nil {
		return fmt.Errorf("init DataPath dir failed, %s", err.Error())
	}

	if err := l.initDb(); err != nil {
		return err
	}
	if err := l.initEmptyDir(); err != nil {
		return err
	}
	if err := l.initHostInfo(); err != nil {
		return err
	}

	return nil
}

func (l *local) CreateCubeboxBaseStorage(ctx context.Context, opts *workflow.CreateContext) (retErr error) {
	templateID, ok := opts.GetSnapshotTemplateID()
	if !ok || templateID == "" {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "templateID is empty")
	}
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
	}

	result := &StorageInfo{Namespace: ns, SandboxID: opts.GetSandboxID(), Volumes: make(map[string]*BackendFileInfo),
		TemplateID: templateID, InstanceType: opts.GetInstanceType()}
	defer func() {
		start := time.Now()
		var errs error
		if err := l.writeBackendFileInfo(ctx, opts.GetSandboxID(), result); err != nil {
			errs = errors.Join(errs, err)
		}

		templateResult := &StorageInfo{Namespace: ns, SandboxID: templateID,
			TemplateID: templateID, InstanceType: opts.GetInstanceType(), CreateAt: time.Now(), UpdateAt: time.Now()}
		if err := l.writeBackendFileInfo(ctx, templateID, templateResult); err != nil {
			errs = errors.Join(errs, err)
		}
		workflow.RecordCreateMetric(ctx, errs, storageMetricDB, time.Since(start))
		if retErr != nil || errs != nil {

			if errs != nil {
				retErr = ret.Err(errorcode.ErrorCode_CreateStorageFailed, errs.Error())
				log.G(ctx).Fatalf("saveBackendFileInfo fail:%v", errs)
			}
			l.Destroy(ctx, &workflow.DestroyContext{
				BaseWorkflowInfo: workflow.BaseWorkflowInfo{
					SandboxID:  opts.GetSandboxID(),
					IsRollBack: true,
				},
				DestroyInfo: &cubebox.DestroyCubeSandboxRequest{
					SandboxID: opts.GetSandboxID(),
				},
			})
			return
		}
		opts.StorageInfo = result
	}()

	defer recov.HandleCrash(func(panicError interface{}) {
		log.G(ctx).Fatalf("Create panic info:%s, stack:%s", panicError, string(debug.Stack()))
		retErr = ret.Errorf(errorcode.ErrorCode_CreateStorageFailed, "%s", panicError)
	})
	for _, v := range opts.ReqInfo.Volumes {
		if v.GetVolumeSource() == nil || v.GetVolumeSource().GetEmptyDir() == nil {
			continue
		}
		if v.GetVolumeSource().GetEmptyDir().GetMedium() == cubebox.StorageMedium_StorageMediumDefault {
			sizeLimit := v.GetVolumeSource().GetEmptyDir().SizeLimit
			sizeq, err := resource.ParseQuantity(sizeLimit)
			if err != nil {
				log.G(ctx).Errorf("invalid EmptyDir SizeLimit")
				return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat,
					"invalid EmptyDir SizeLimit:%v", sizeLimit)
			}
			info := &BackendFileInfo{
				Name:       v.Name,
				SizeLimit:  sizeq.Value() + diskSizeOverheadInBytes,
				Type:       "ext4",
				SourcePath: "disk",
				SizeLimitQ: sizeq.String(),
				FSQuota:    sizeq.Value() + diskSizeExtendInBytes,
				Medium:     cubebox.StorageMedium_StorageMediumDefault,
			}
			result.Volumes[v.Name] = info
			info.FilePath, err = l.newCubeboxFormatPool(ctx, templateID, sizeLimit)
			if err != nil {
				return ret.Err(errorcode.ErrorCode_CreateStorageFailed, err.Error())
			}
			return nil
		}
	}

	return ret.Err(errorcode.ErrorCode_CreateStorageFailed, "not found default medium")
}

func (l *local) Create(ctx context.Context, opts *workflow.CreateContext) (retErr error) {
	select {

	case <-ctx.Done():
		return
	default:
	}
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.CreateContext nil")
	}
	realReq := opts.ReqInfo

	if opts.IsCreateSnapshot() && !opts.IsCubeboxV2() {

		info, err := l.readBackendFileInfo(ctx, opts.GetSandboxID())
		if err == nil && info.SandboxID == opts.GetSandboxID() {
			return ret.Err(errorcode.ErrorCode_PreConditionFailed, "already exists")
		}
		return l.CreateCubeboxBaseStorage(ctx, opts)
	}

	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
	}

	result := &StorageInfo{Namespace: ns, SandboxID: opts.SandboxID, Volumes: make(map[string]*BackendFileInfo)}
	defer func() {
		start := time.Now()
		err := l.writeBackendFileInfo(ctx, opts.SandboxID, result)
		workflow.RecordCreateMetric(ctx, err, storageMetricDB, time.Since(start))
		if retErr != nil || err != nil {

			if err != nil {
				retErr = ret.Err(errorcode.ErrorCode_CreateStorageFailed, err.Error())
				log.G(ctx).Fatalf("saveBackendFileInfo fail:%v", err)
			}
			l.Destroy(ctx, &workflow.DestroyContext{
				BaseWorkflowInfo: workflow.BaseWorkflowInfo{
					SandboxID:  opts.SandboxID,
					IsRollBack: true,
				},
			})
			return
		}

		templateID, ok := opts.GetSnapshotTemplateID()
		if ok && !opts.IsCubeboxV2() {

			err = l.updateCubeBoxBaseInfo(ctx, templateID)
			if err != nil {
				log.G(ctx).Fatalf("updateCubeBoxBaseInfo fail:%v", err)
			}
		}
		opts.StorageInfo = result
	}()
	defer recov.HandleCrash(func(panicError interface{}) {
		log.G(ctx).Fatalf("Create panic info:%s, stack:%s", panicError, string(debug.Stack()))
		retErr = ret.Errorf(errorcode.ErrorCode_CreateStorageFailed, "%s", panicError)
	})
	for _, v := range realReq.Volumes {
		if v.VolumeSource == nil {
			continue
		}
		if err = l.prepareDefaultMedium(ctx, opts, v, result); err != nil {
			return ret.WrapWithDefaultError(err, errorcode.ErrorCode_CreateStorageFailed)
		}

		if err = l.prepareImageVolume(ctx, opts, v, result); err != nil {
			return ret.WrapWithDefaultError(err, errorcode.ErrorCode_CreateStorageFailed)
		}

		if err = l.prepareHostDirVolume(ctx, opts, v, result); err != nil {
			return ret.WrapWithDefaultError(err, errorcode.ErrorCode_CreateStorageFailed)
		}
	}

	return nil
}

func (l *local) prepareDefaultMedium(ctx context.Context, opts *workflow.CreateContext,
	v *cubebox.Volume, result *StorageInfo) error {
	if v.GetVolumeSource() == nil || v.GetVolumeSource().GetEmptyDir() == nil ||
		v.GetVolumeSource().GetEmptyDir().GetMedium() != cubebox.StorageMedium_StorageMediumDefault {
		return nil
	}
	sizeLimit := v.GetVolumeSource().GetEmptyDir().SizeLimit

	if id, ok := opts.GetSnapshotTemplateID(); ok && !opts.IsCubeboxV2() {
		if err := l.dealCubeboxSnapV1Medium(ctx, id, v.Name, sizeLimit, result); err != nil {
			return ret.WrapWithDefaultError(err, errorcode.ErrorCode_CreateStorageFailed)
		}
	} else if err := l.dealDefaultMedium(ctx, v.Name, sizeLimit, result); err != nil {
		return ret.WrapWithDefaultError(err, errorcode.ErrorCode_CreateStorageFailed)
	}
	return nil
}

func (l *local) dealCubeboxSnapV1Medium(ctx context.Context, templateID, name, sizeStr string, result *StorageInfo) error {
	isOk := l.checkDiskAvailable(ctx)
	if !isOk && !l.config.DisableDiskCheck {
		return fmt.Errorf("disk exceed limit")
	}
	size, err := resource.ParseQuantity(sizeStr)
	log.G(ctx).Debugf("req GetEmptyDir:%+v,vName:%s", sizeStr, name)
	if err != nil {
		log.G(ctx).Errorf("invalid EmptyDir SizeLimit: %s", sizeStr)
		return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "invalid EmptyDir SizeLimit:%v", size)
	}

	p, ok := l.poolFormat.Load(templateID)
	if !ok {
		return fmt.Errorf("failed to get v1 template storage pool for templateID %s", templateID)
	}
	pool := p.(Pool)
	if devInfo, err := pool.Get(ctx, 0); err != nil {
		log.G(ctx).Errorf("allocate cubebox storage fail:%v", err)
		return ret.Errorf(errorcode.ErrorCode_CreateStorageFailed, "allocate cubebox storage fail:%v", err)
	} else {
		info := &BackendFileInfo{
			Name:       name,
			FilePath:   devInfo.FilePath,
			SizeLimit:  size.Value() + diskSizeOverheadInBytes,
			Type:       "ext4",
			SourcePath: "disk",
			SizeLimitQ: size.String(),
			FSQuota:    size.Value() + diskSizeExtendInBytes,
			Medium:     cubebox.StorageMedium_StorageMediumDefault,
		}
		result.Volumes[name] = info
		return nil
	}
}

func (l *local) dealDefaultMedium(ctx context.Context, name, sizeStr string, result *StorageInfo) error {
	size, err := resource.ParseQuantity(sizeStr)
	log.G(ctx).Debugf("req GetEmptyDir:%+v,vName:%s", sizeStr, name)
	if err != nil {
		log.G(ctx).Errorf("invalid EmptyDir SizeLimit: %s", sizeStr)
		return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "invalid EmptyDir SizeLimit:%v", size)
	}
	allocateSize := unifiedStorageSize
	if size.Value() > unifiedStorageSize.Value() {
		allocateSize = size
	}

	if devInfo, err := l.getDevInfo(ctx, allocateSize); err != nil {
		log.G(ctx).Errorf("allocate storage fail:%v", err)
		return ret.Errorf(errorcode.ErrorCode_CreateStorageFailed, "allocate storage fail:%v", err)
	} else {
		info := &BackendFileInfo{
			Name:       name,
			FilePath:   devInfo.FilePath,
			SizeLimit:  allocateSize.Value() + diskSizeOverheadInBytes,
			Type:       "ext4",
			SourcePath: "disk",
			SizeLimitQ: allocateSize.String(),
			FSQuota:    size.Value() + diskSizeExtendInBytes,
			Medium:     cubebox.StorageMedium_StorageMediumDefault,
		}
		result.Volumes[name] = info
		return nil
	}
}

func (l *local) getDevInfo(ctx context.Context, q resource.Quantity) (*devInfo, error) {
	isOk := l.checkDiskAvailable(ctx)
	if !isOk && !l.config.DisableDiskCheck {
		return nil, fmt.Errorf("disk exceed limit")
	}

	if p, ok := l.poolFormat.Load(q.String()); ok {
		return p.(Pool).Get(ctx, 0)
	} else if p, ok := l.poolFormat.Load(otherFormatSize); ok {
		return p.(Pool).GetSync(ctx, q.Value())
	}
	return nil, fmt.Errorf("failed to get default medium storage device info for size %s", q.String())
}

func (l *local) Destroy(ctx context.Context, opts *workflow.DestroyContext) (err error) {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.DestroyContext nil")
	}
	log.G(ctx).Debugf("Destroy doing")
	start := time.Now()
	info, err := l.readBackendFileInfo(ctx, opts.SandboxID)
	workflow.RecordDestroyMetric(ctx, err, storageMetricDB, time.Since(start))
	if err != nil {

		if errors.Is(err, utils.ErrorKeyNotFound) || errors.Is(err, utils.ErrorBucketNotFound) {
			return nil
		}
		log.G(ctx).Fatalf("readBackendFileInfo fail:%v", err)
		return ret.Err(errorcode.ErrorCode_DestroyStorageFailed, err.Error())
	}
	defer func() {
		if err == nil {

			l.deleteBackendFileInfo(ctx, opts.SandboxID)
		}
	}()

	log.G(ctx).Debugf("Destroy_info:%s", utils.InterfaceToString(info))
	return l.destroy(ctx, info, opts)
}

func (l *local) destroy(ctx context.Context, info *StorageInfo, opts *workflow.DestroyContext) error {
	isRollback := false
	if opts != nil {
		isRollback = opts.IsRollBack
	}
	err := cdp.PreDelete(ctx, &cdp.DeleteOption{
		ID:                  info.SandboxID,
		ResourceType:        cdp.ResourceDeleteProtectionTypeStorage,
		SkipDeleteFlagCheck: isRollback,
	})
	if err != nil {
		return fmt.Errorf("storage destroy: pre delete storage failed: %w", err)
	}

	var errs error
	if err := l.destroyCubeBoxTemplateBase(ctx, info, opts); err != nil {
		errs = errors.Join(errs, err)
	}

	if err := l.destroyDefaultMediumVolumes(ctx, info); err != nil {
		errs = errors.Join(errs, err)
	}

	if errs != nil {
		return ret.Err(errorcode.ErrorCode_DestroyStorageFailed, errs.Error())
	}
	return nil
}

func (l *local) destroyDefaultMediumVolumes(ctx context.Context, info *StorageInfo) error {
	var errs error
	for _, v := range info.Volumes {
		err := atomicDelete(v.FilePath)
		if err != nil {
			errs = errors.Join(errs, err)
			log.G(ctx).Fatalf("delete:%s,fail:%v", v.FilePath, err)
		} else {
			l.DecrSize(v.SizeLimit)
		}
	}
	return errs
}

func (l *local) destroyCubeBoxTemplateBase(ctx context.Context, info *StorageInfo, opts *workflow.DestroyContext) error {
	if opts == nil {
		return nil
	}

	if info.InstanceType != cubebox.InstanceType_cubebox.String() ||
		info.TemplateID == "" || info.SandboxID == info.TemplateID ||
		l.config.PoolType != cp_reflink_type {
		return nil
	}

	templateID := info.TemplateID

	log.G(ctx).Debugf("destroyCubeBoxTemplateBase:%s", info.TemplateID)
	if ctx.Value(cleanupCtx{}) != nil || opts.IsRollBack {

		log.G(ctx).Warnf("destroyCubeBoxTemplateBase:%s from cleanup/failover, no need to init base file", templateID)
		l.tmpPoolFormat.Delete(templateID)
		return nil
	}

	p, ok := l.tmpPoolFormat.Load(templateID)
	if !ok {
		log.G(ctx).Errorf("templateID not found: %s", templateID)
		return ret.Errorf(errorcode.ErrorCode_AppSnapshotNotExist, "templateID not found: %s", templateID)
	}
	defer l.tmpPoolFormat.Delete(templateID)
	if err := p.(Pool).InitBaseFile(ctx); err != nil {
		return err
	}
	l.poolFormat.Store(templateID, p)
	return nil
}

func (l *local) updateBlocksCap() {
	capBlocks, capInodes, err := utils.GetDeviceIdleRatio(l.config.DataPath)
	if err == nil {
		l.capBlocks.Store(int32(capBlocks))
		l.capInodes.Store(int32(capInodes))
	}
}
func (l *local) CleanUp(ctx context.Context, opts *workflow.CleanContext) error {
	log.G(ctx).Errorf("CleanUp doing")
	sandBoxID := opts.SandboxID
	ctx = context.WithValue(ctx, cleanupCtx{}, struct{}{})
	if err := l.Destroy(ctx, &workflow.DestroyContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: sandBoxID,
		},
	}); err != nil {

		log.G(ctx).Errorf("CleanUp fail:%v", err)
		return err
	}

	return nil
}

func (l *local) failoverDir() string {
	return filepath.Join(l.config.RootPath, failoverDir)
}

func (l *local) deleteBackendFileInfo(ctx context.Context, id string) error {

	err := l.db.Delete(bucketName, id)
	if err != nil {
		log.G(ctx).Warnf("db delete id :%s,err:%s", id, err)
		err = atomicDelete(filepath.Join(l.failoverDir(), id))
		if err != nil {
			log.G(ctx).Warnf("db delete failover id :%s,err:%s", id, err)
		}
	}
	return err
}

func (l *local) readBackendFileInfo(ctx context.Context, id string) (*StorageInfo, error) {
	b, err := l.db.Get(bucketName, id)
	if err != nil {
		if errors.Is(err, utils.ErrorKeyNotFound) || errors.Is(err, utils.ErrorBucketNotFound) {
			fileName := filepath.Join(l.failoverDir(), id)
			if fileName == l.failoverDir() {

				return nil, utils.ErrorKeyNotFound
			}
			if exist, _ := utils.DenExist(fileName); exist {
				b, err = os.ReadFile(fileName)
				if err != nil {
					return nil, err
				}
			} else {
				return nil, utils.ErrorKeyNotFound
			}
		} else {
			return nil, utils.ErrorKeyNotFound
		}
	}
	bf := &StorageInfo{}
	err = json.Unmarshal(b, bf)
	if err != nil {
		return nil, err
	}
	return bf, nil
}

func (l *local) writeBackendFileInfo(ctx context.Context, id string, info *StorageInfo) error {
	b, _ := json.Marshal(info)
	err := l.db.Set(bucketName, id, b)
	if err != nil {

		er := os.WriteFile(filepath.Join(l.failoverDir(), id), b, 0666)
		if er != nil {
			_ = atomicDelete(filepath.Join(l.failoverDir(), id))
			return fmt.Errorf("%w:%s", err, er)
		}
	}
	return nil
}

func (l *local) updateCubeBoxBaseInfo(ctx context.Context, templateID string) error {
	info, err := l.readBackendFileInfo(ctx, templateID)
	if err != nil {
		if errors.Is(err, utils.ErrorKeyNotFound) || errors.Is(err, utils.ErrorBucketNotFound) {
			return nil
		}
		return err
	}
	info.UpdateAt = time.Now()
	b, _ := json.Marshal(info)
	err = l.db.Set(bucketName, templateID, b)
	if err != nil {

		er := os.WriteFile(filepath.Join(l.failoverDir(), templateID), b, 0666)
		if er != nil {
			_ = atomicDelete(filepath.Join(l.failoverDir(), templateID))
			return fmt.Errorf("%w:%s", err, er)
		}
	}
	return nil
}

func (l *local) readAllFileInfo() (map[string][]byte, error) {
	all, _ := l.db.ReadAll(bucketName)
	denList, err := ReadDir(l.failoverDir())
	if err != nil {
		return all, nil
	}
	for _, den := range denList {
		if den.IsDir() {
			continue
		}
		filePath := filepath.Join(l.failoverDir(), den.Name())
		b, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}
		if all == nil {
			all = map[string][]byte{}
		}
		all[den.Name()] = b
	}
	return all, nil
}

func (l *local) readAllStorageInfo() (map[string]*StorageInfo, error) {
	all, err := l.readAllFileInfo()
	if err != nil {
		return nil, err
	}
	storageInfo := make(map[string]*StorageInfo)
	for id, data := range all {
		info := &StorageInfo{}
		err := json.Unmarshal(data, info)
		if err != nil {
			continue
		}
		storageInfo[id] = info
	}
	return storageInfo, nil
}

func (l *local) incrSize(s int64) {
	l.usedDiskSize.Add(s)
}

func (l *local) DecrSize(s int64) {
	l.usedDiskSize.Add(-s)
}

func (l *local) checkDiskAvailable(ctx context.Context) bool {

	if l.capBlocks.Load() != 0 && l.capInodes.Load() != 0 {
		if l.capBlocks.Load() < l.config.FreeBlocksThreshold || l.capInodes.Load() < l.config.FreeInodesThreshold {
			log.G(ctx).Fatalf("Storage freeBlocks:%d freeInodes:%d is exceed (%d:%d)",
				l.capBlocks.Load(), l.capInodes.Load(), l.config.FreeBlocksThreshold, l.config.FreeInodesThreshold)
			return false
		}
	}
	return true
}

func GetPCIDiskInfo(ctx context.Context, id string) (*disk.CubePCIDiskInfo, error) {
	if localStorage == nil {
		return nil, nil
	}
	info, err := localStorage.readBackendFileInfo(ctx, id)
	if err != nil && !errors.Is(err, utils.ErrorKeyNotFound) {
		CubeLog.WithContext(ctx).Errorf("GetPCIDiskInfo fail %w", err)
		return nil, err
	}
	if errors.Is(err, utils.ErrorKeyNotFound) {
		return nil, nil
	}
	return info.CubePCIDiskInfo, nil
}

type BackendFileInfo struct {
	Name string

	SizeLimit int64

	FSQuota int64

	FilePath string

	Type string

	SourcePath string

	SizeLimitQ string

	Medium cubebox.StorageMedium

	BDF string
}

type StorageInfo struct {
	Namespace string

	SandboxID string
	Volumes   map[string]*BackendFileInfo

	CubePCIDiskInfo *disk.CubePCIDiskInfo

	CubePCISystemDiskInfo *disk.CubePCISystemDiskInfo

	InstanceType string    `json:"instanceType,omitempty"`
	TemplateID   string    `json:"templateId,omitempty"`
	CreateAt     time.Time `json:"createAt,omitempty"`
	UpdateAt     time.Time `json:"updateAt,omitempty"`

	HostDirBackendInfos map[string]*HostDirBackendInfo `json:"hostDirBackendInfos,omitempty"`
}

func (i *StorageInfo) GetNICQueues() int64 {
	var queues int64 = 0

	if i.CubePCIDiskInfo != nil && i.CubePCIDiskInfo.Queues > 0 {
		queues = i.CubePCIDiskInfo.Queues
	}

	if i.CubePCISystemDiskInfo != nil && i.CubePCISystemDiskInfo.Queues > 0 {
		queues += i.CubePCISystemDiskInfo.Queues
	}

	return queues
}

func atomicDelete(path string) error {

	atomicPath := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s", filepath.Base(path)))
	if err := os.Rename(path, atomicPath); err != nil && !os.IsExist(err) {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.RemoveAll(atomicPath)
}

func ReadDir(name string) ([]os.DirEntry, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dirs, err := f.ReadDir(-1)
	return dirs, err
}

func (l *local) ensureSymlink(ctx context.Context, uniquePath string, targetPath string) error {
	lock := l.multiLock.Get(uniquePath)
	lock.Lock()
	defer lock.Unlock()

	if linkInfo, err := os.Lstat(uniquePath); err == nil {
		if linkInfo.Mode()&os.ModeSymlink != 0 {

			target, err := os.Readlink(uniquePath)
			if err == nil && target == targetPath {
				log.G(ctx).Debugf("symlink already exists: %s -> %s", uniquePath, target)
				return nil
			}

			log.G(ctx).Warnf("symlink target mismatch, removing: %s", uniquePath)
			if err := os.Remove(uniquePath); err != nil {
				return fmt.Errorf("failed to remove old symlink: %w", err)
			}
		} else {

			return fmt.Errorf("path exists but is not a symlink: %s", uniquePath)
		}
	}

	parentDir := filepath.Dir(uniquePath)

	if _, err := os.Stat(parentDir); os.IsNotExist(err) {

		if err := os.MkdirAll(parentDir, os.ModeDir|0755); err != nil {
			return fmt.Errorf("failed to create parent directory %s: %w", parentDir, err)
		}
	}

	if err := os.Symlink(targetPath, uniquePath); err != nil {
		return fmt.Errorf("failed to create symlink from %s to %s: %w", uniquePath, targetPath, err)
	}

	log.G(ctx).Infof("created symlink: %s -> %s", uniquePath, targetPath)
	return nil
}

func (l *local) readStorageInfo(ctx context.Context, id string, bucketName string, failoverDirFunc func() string, infoType interface{}) error {
	b, err := l.db.Get(bucketName, id)
	if err != nil {
		if errors.Is(err, utils.ErrorKeyNotFound) || errors.Is(err, utils.ErrorBucketNotFound) {
			fileName := filepath.Join(failoverDirFunc(), id)
			if fileName == failoverDirFunc() {

				return utils.ErrorKeyNotFound
			}
			if exist, _ := utils.DenExist(fileName); exist {
				b, err = os.ReadFile(fileName)
				if err != nil {
					return err
				}
			} else {
				return utils.ErrorKeyNotFound
			}
		} else {
			return utils.ErrorKeyNotFound
		}
	}
	err = json.Unmarshal(b, infoType)
	if err != nil {
		return err
	}
	return nil
}

func (l *local) writeStorageInfo(ctx context.Context, id string, info interface{}, bucketName string, failoverDirFunc func() string) error {
	b, _ := json.Marshal(info)
	err := l.db.Set(bucketName, id, b)
	if err != nil {

		er := os.WriteFile(filepath.Join(failoverDirFunc(), id), b, 0666)
		if er != nil {
			if delErr := atomicDelete(filepath.Join(failoverDirFunc(), id)); delErr != nil {
				return fmt.Errorf("write file failed: %w, delete failed: %v", er, delErr)
			}
			return fmt.Errorf("%w:%s", err, er)
		}
	}
	return nil
}

func (l *local) deleteStorageInfo(ctx context.Context, id string, bucketName string, failoverDirFunc func() string) error {
	err := l.db.Delete(bucketName, id)
	if err != nil {
		log.G(ctx).Warnf("db delete id :%s,err:%s", id, err)
		delErr := atomicDelete(filepath.Join(failoverDirFunc(), id))
		if delErr != nil {
			log.G(ctx).Warnf("db delete failover id :%s,err:%s", id, delErr)
		}
	}
	return err
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package images

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"time"

	"encoding/json"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/multimetadb/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/multilock"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/volumefile"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/multimeta"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/cubelog"
	"golang.org/x/sync/errgroup"
)

type volumeLocal struct {
	config         *VolumeConfig
	lifetime       *volumeLifetime
	db             *utils.CubeStore
	codeMultiLock  *multilock.MultiLock
	layerMultiLock *multilock.MultiLock
	langMultiLock  *multilock.MultiLock
}

const (
	volumeDbDir          = "volumedb"
	bucketName           = "createInfo"
	volumeCodeMetric     = "cube-volume-code"
	volumeLayerMetric    = "cube-volume-layer"
	volumeLangMetric     = "cube-volume-lang"
	volumeLangExt4Metric = "cube-volume-lang_ext4"
)

var (
	registerVolumeBucket = multimeta.BucketDefineInternal{
		BucketDefine: &multimetadb.BucketDefine{
			Name:     bucketName,
			DbName:   "cubebox-volume",
			Describe: "cubebox service volume metadata db",
		},
	}
)

func (l *volumeLocal) volumeDBDir() string {
	return filepath.Join(l.config.RootPath, volumeDbDir)
}

func (l *volumeLocal) baseVolumeDownloadDir(ft volumefile.FileType) string {
	return filepath.Join(l.config.DataPath, fmt.Sprintf("%s_backfile", getBucketName(ft)))
}

func (l *volumeLocal) baseVolumeDir(ft volumefile.FileType) string {
	return filepath.Join(l.config.DataPath, getBucketName(ft))
}

func (l *volumeLocal) ID() string {
	return constants.VolumeSourceID.ID()
}
func (l *volumeLocal) Init(ctx context.Context, opts *workflow.InitInfo) error {
	return nil
}

func (l *volumeLocal) Close() error {

	l.lifetime.syncFlush(context.Background())
	return nil
}

func (l *volumeLocal) init(ctx context.Context) error {
	if err := l.initDb(); err != nil {
		return err
	}
	l.codeMultiLock = multilock.NewMultiLock(multilock.NewMultiLockOptions())
	l.layerMultiLock = multilock.NewMultiLock(multilock.NewMultiLockOptions())
	l.langMultiLock = multilock.NewMultiLock(multilock.NewMultiLockOptions())
	l.lifetime = &volumeLifetime{
		conf:           l.config,
		volumeLocalPtr: l,
	}
	if err := l.lifetime.init(ctx); err != nil {
		return err
	}
	return nil
}
func (l *volumeLocal) initDb() error {
	var err error
	if l.db, err = utils.NewCubeStoreExt(l.volumeDBDir(), "meta.db", 10, nil); err != nil {
		return err
	}

	registerVolumeBucket.CubeStore = l.db
	multimeta.RegisterBucket(&registerVolumeBucket)
	return nil
}

func (l *volumeLocal) Create(ctx context.Context, opts *workflow.CreateContext) (retErr error) {
	select {

	case <-ctx.Done():
		return
	default:
	}
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.CreateContext nil")
	}
	realReq := opts.ReqInfo
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
	}

	result := &Info{Namespace: ns, SandboxID: opts.SandboxID, Volumes: make(map[string]*BackendFileInfo)}
	tmpInfo := &createInfo{
		Timestamp: time.Now().Unix(),
	}
	defer func() {
		if retErr == nil && len(result.Volumes) > 0 {
			opts.VolumeInfo = result
			if len(tmpInfo.VolumeInfos) > 0 {
				for _, v := range tmpInfo.VolumeInfos {
					l.lifetime.Add(&meta{
						fileType:   v.FileType,
						userID:     v.UserID,
						fileSha256: v.FileSha256,
						Ref:        1,
						Timestamp:  time.Now().Unix(),
					})
				}
				if err := l.writeVolumeInfo(ctx, opts.SandboxID, tmpInfo); err != nil {
					log.G(ctx).Fatalf("writeVolumeInfo,fail:%v", err)
				}
			}
		}
		log.G(ctx).Debugf("Create end")
	}()

	defer recov.HandleCrash(func(panicError interface{}) {
		log.G(ctx).Fatalf("Create volume panic info:%s, stack:%s", panicError, string(debug.Stack()))
		retErr = ret.Errorf(errorcode.ErrorCode_CreateVolumeFailed, "%s", panicError)
	})
	log.G(ctx).Debugf("Create doing")
	eg, ctxWithCancel := errgroup.WithContext(ctx)
	_ = ctxWithCancel
	for range realReq.Volumes {
	}
	if err := eg.Wait(); err != nil {
		return err
	}
	return nil
}

func (l *volumeLocal) Destroy(ctx context.Context, opts *workflow.DestroyContext) error {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.DestroyContext nil")
	}
	log.G(ctx).Debugf("Destroy doing")
	info, err := l.readVolumeInfo(ctx, opts.SandboxID)
	if err != nil {

		if errors.Is(err, utils.ErrorKeyNotFound) || errors.Is(err, utils.ErrorBucketNotFound) {
			return nil
		}
	}

	for k, v := range info.VolumeInfo {
		info.VolumeInfos = append(info.VolumeInfos, volumeInfo{
			FileType:   k,
			UserID:     info.UserID,
			FileSha256: v,
		})
	}

	for _, v := range info.VolumeInfos {
		l.lifetime.Add(&meta{
			fileType:   v.FileType,
			userID:     v.UserID,
			fileSha256: v.FileSha256,
			Ref:        -1,
			Timestamp:  time.Now().Unix(),
		})
		appID, _ := strconv.ParseInt(v.UserID, 10, 64)
		rt := CubeLog.GetTraceInfo(ctx)
		if rt != nil {
			rt = rt.DeepCopy()
			rt.Cost = time.Duration(time.Now().Unix()-info.Timestamp) * time.Second
			rt.Callee = volumeDbDir
			rt.AppID = appID
			rt.CalleeEndpoint = v.FileSha256
			rt.CalleeAction = getCalleeAction(v.FileType)
			CubeLog.Trace(rt)
		}
	}
	return nil
}

func (l *volumeLocal) CleanUp(ctx context.Context, opts *workflow.CleanContext) error {
	if opts == nil {
		return nil
	}
	log.G(ctx).Errorf("CleanUp doing")
	info, err := l.readVolumeInfo(ctx, opts.SandboxID)
	if err != nil {

		if errors.Is(err, utils.ErrorKeyNotFound) || errors.Is(err, utils.ErrorBucketNotFound) {
			return nil
		}
	}

	for k, v := range info.VolumeInfo {
		info.VolumeInfos = append(info.VolumeInfos, volumeInfo{
			FileType:   k,
			UserID:     info.UserID,
			FileSha256: v,
		})
	}
	for _, v := range info.VolumeInfos {
		l.lifetime.Add(&meta{
			fileType:   v.FileType,
			userID:     v.UserID,
			fileSha256: v.FileSha256,
			Ref:        -1,
			Timestamp:  time.Now().Unix(),
		})
		appID, _ := strconv.ParseInt(v.UserID, 10, 64)
		rt := CubeLog.GetTraceInfo(ctx)
		if rt != nil {
			rt = rt.DeepCopy()
			rt.Cost = time.Duration(time.Now().Unix()-info.Timestamp) * time.Second
			rt.Callee = volumeDbDir
			rt.AppID = appID
			rt.CalleeEndpoint = v.FileSha256
			rt.CalleeAction = getCalleeAction(v.FileType)
			CubeLog.Trace(rt)
		}
	}
	return nil
}

func (l *volumeLocal) writeVolumeInfo(ctx context.Context, id string, info *createInfo) error {
	b, _ := json.Marshal(info)
	err := l.db.Set(bucketName, id, b)
	if err != nil {
		return err
	}
	return nil
}

func (l *volumeLocal) readVolumeInfo(ctx context.Context, id string) (*createInfo, error) {
	b, err := l.db.Get(bucketName, id)
	if err != nil {
		return nil, err
	}

	_ = l.db.Delete(bucketName, id)

	bf := &createInfo{}
	err = json.Unmarshal(b, bf)
	if err != nil {
		return nil, err
	}
	return bf, nil
}

func (l *volumeLocal) SetExpirationTime(t time.Duration) error {
	if t.Hours() < 24 {
		return fmt.Errorf("too short expiration time, must be greater or equal than 24 hours")
	}
	req := int64(t.Seconds())
	if req != l.lifetime.conf.ExpiredInSecond {
		l.lifetime.conf.ExpiredInSecond = req
		CubeLog.Errorf("set code expiration time to %v", t)
	}

	return nil
}

type volumeInfo struct {
	FileType   volumefile.FileType
	UserID     string
	FileSha256 string
}

type createInfo struct {
	UserID string

	VolumeInfo map[volumefile.FileType]string

	VolumeInfos []volumeInfo
	Timestamp   int64
}

type BackendFileInfo struct {
	Name string

	FilePath string

	FileType volumefile.FileType
}

type Info struct {
	Namespace string

	SandboxID string
	Volumes   map[string]*BackendFileInfo
}

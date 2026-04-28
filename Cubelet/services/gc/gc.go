// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package gc

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin/registry"
	//"encoding/json"
	"encoding/json"

	"github.com/moby/sys/mountinfo"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/plugin"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/multimetadb/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/multimeta"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type GCConfig struct {
	RootPath string `toml:"root_path"`
}

var l = &local{}

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.InternalPlugin,
		ID:     constants.GCID.ID(),
		Config: &GCConfig{},
		Requires: []plugin.Type{
			constants.CubeStorePlugin,
		},
		InitFn: func(ic *plugin.InitContext) (_ interface{}, err error) {
			defer func() {
				if err != nil {
					CubeLog.Fatalf("plugin %s init fail:%v", constants.GCID, err.Error())
				}
			}()
			config := ic.Config.(*GCConfig)
			if config.RootPath == "" {
				config.RootPath = ic.Properties[plugins.PropertyStateDir]
			}

			if err := os.MkdirAll(path.Clean(config.RootPath), os.ModeDir|0755); err != nil {
				return nil, fmt.Errorf("init RootPath dir failed, %s", err.Error())
			}
			l.config = config

			if err := l.initDb(); err != nil {
				return nil, err
			}

			cubeboxAPIObj, err := ic.GetByID(constants.CubeStorePlugin, constants.CubeboxID.ID())
			if err != nil {
				return nil, fmt.Errorf("get cubebox api client fail:%v", err)
			}
			l.cubeboxManger = cubeboxAPIObj.(cubes.CubeboxAPI)
			return l, nil
		},
	})
}

type local struct {
	config        *GCConfig
	db            *utils.CubeStore
	cubeboxManger cubes.CubeboxAPI
}

var (
	dbDir      = "db"
	bucketName = "sandbox/v1"

	registerBucket = multimeta.BucketDefineInternal{
		BucketDefine: &multimetadb.BucketDefine{
			Name:     bucketName,
			DbName:   "gcservice",
			Describe: "gc service db to store sandbox info",
		},
	}
)

func (l *local) initDb() error {
	basePath := filepath.Join(l.config.RootPath, dbDir)
	if err := os.MkdirAll(path.Clean(basePath), os.ModeDir|0755); err != nil {
		return fmt.Errorf("init dir failed %s", err.Error())
	}
	var err error
	if l.db, err = utils.NewCubeStoreExt(basePath, "meta.db", 10, nil); err != nil {
		return err
	}

	registerBucket.CubeStore = l.db
	multimeta.RegisterBucket(&registerBucket)
	return nil
}

func (l *local) ID() string {
	return constants.GCID.ID()
}

func (l *local) Init(ctx context.Context, opts *workflow.InitInfo) error {
	log.G(ctx).Errorf("Init doing")
	defer log.G(ctx).Errorf("Init end")
	_ = l.db.Close()
	time.Sleep(time.Second)

	_ = mount.UnmountAll(l.config.RootPath, 0)
	if err := os.RemoveAll(path.Clean(l.config.RootPath)); err != nil {
		log.G(ctx).Infof("init fail,RemoveAll err:%v", err)
		return err
	}

	if err := os.MkdirAll(path.Clean(l.config.RootPath), os.ModeDir|0755); err != nil {
		return fmt.Errorf("init RootPath dir failed, %s", err.Error())
	}

	size := 100
	m := &mount.Mount{
		Type:    "tmpfs",
		Source:  "none",
		Options: []string{fmt.Sprintf("size=%dm", size)},
	}
	if err := m.Mount(l.config.RootPath); err != nil {
		return err
	}
	exist, _ := mountinfo.Mounted(l.config.RootPath)
	if !exist {
		return fmt.Errorf("mount tmpfs:%v fail", l.config.RootPath)
	}

	if err := l.initDb(); err != nil {
		return err
	}
	return nil
}

func (l *local) Create(ctx context.Context, opts *workflow.CreateContext) error {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.CreateContext nil")
	}
	log.G(ctx).Errorf("Create doing")
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
	}
	info := &sandBoxInfo{
		SandboxID: opts.SandboxID,
		Namespace: ns,
	}
	if err := l.saveSandBoxInfo(info); err != nil {
		log.G(ctx).Warnf("saveSandBoxInfo failed:%s", err.Error())
		return ret.Err(errorcode.ErrorCode_UpdateLocalMetaDataFailed, err.Error())
	}

	cb, err := l.cubeboxManger.Get(ctx, opts.SandboxID)
	if err == nil {
		if cb.UserMarkDeletedTime == nil {
			now := time.Now()
			cb.UserMarkDeletedTime = &now
			l.cubeboxManger.SyncByID(ctx, opts.SandboxID)
		}
	}
	return nil
}

func (l *local) Destroy(ctx context.Context, opts *workflow.DestroyContext) error {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.Destroy nil")
	}
	log.G(ctx).Debugf("Destroy doing")
	if err := l.deleteSandBoxInfo(opts.SandboxID); err != nil {
		log.G(ctx).Warnf("deleteSandBoxInfo failed:%s", err.Error())
		return ret.Err(errorcode.ErrorCode_UpdateLocalMetaDataFailed, err.Error())
	}
	return nil
}

func (l *local) CleanUp(ctx context.Context, opts *workflow.CleanContext) error {
	if opts == nil {
		return nil
	}
	log.G(ctx).Errorf("CleanUp doing")
	if err := l.deleteSandBoxInfo(opts.SandboxID); err != nil {
		log.G(ctx).Errorf("deleteSandBoxInfo failed:%s", err.Error())
		return ret.Err(errorcode.ErrorCode_UpdateLocalMetaDataFailed, err.Error())
	}
	return nil
}

type sandBoxInfo struct {
	SandboxID string
	Namespace string
}

func (l *local) saveSandBoxInfo(info *sandBoxInfo) error {
	b, _ := json.Marshal(info)
	return l.db.Set(bucketName, info.SandboxID, b)
}

func (l *local) deleteSandBoxInfo(sandBoxID string) error {
	if err := l.db.Delete(bucketName, sandBoxID); err != utils.ErrorKeyNotFound &&
		err != utils.ErrorBucketNotFound {
		return err
	}
	return nil
}

func (l *local) readAll() (infos []*sandBoxInfo, _ error) {
	all, err := l.db.ReadAll(bucketName)
	if err != nil {
		return nil, err
	}

	for k, v := range all {
		bf := &sandBoxInfo{}
		err = json.Unmarshal(v, bf)
		if err != nil {
			CubeLog.Warnf("readAll [%s] failed:%s", k, err.Error())
			continue
		}
		infos = append(infos, bf)
	}

	return infos, nil
}

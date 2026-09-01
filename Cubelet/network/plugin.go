// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/google/uuid"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/multimetadb/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	networkstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/network"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/multimeta"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type delegateNetworkManager struct {
	tapPlugin       *local
	config          Config
	db              *utils.CubeStore
	allocationStore *networkstore.Store
}

var dnm *delegateNetworkManager

const DBBucketNetwork = "network/v1"
const (
	networkMetricCreate = "cube-network-create"
	networkMetricGetBdf = "cube-network-bdf"
)

var registerBucket = multimeta.BucketDefineInternal{
	BucketDefine: &multimetadb.BucketDefine{
		Name:     DBBucketNetwork,
		DbName:   "network",
		Describe: "network plugin db",
	},
}

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.InternalPlugin,
		ID:     constants.NetworkID.ID(),
		Config: &Config{},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			tapPlugin, err := initTapPlugin(ic)
			if err != nil {
				log.G(ic.Context).Fatalf("plugin %s init failed: %+v", constants.NetworkID, err)
				return nil, err
			}

			m := &delegateNetworkManager{
				tapPlugin: tapPlugin,
				config:    *ic.Config.(*Config),
			}
			if m.config.RootPath == "" {
				m.config.RootPath = ic.Properties[plugins.PropertyStateDir]
			}
			if err = m.initDB(); err != nil {
				return nil, err
			}
			m.allocationStore, err = networkstore.RecoverFromDB(m.db)
			if err != nil {
				return nil, err
			}
			tapPlugin.SetAllocationStore(m.allocationStore)
			dnm = m
			return m, nil
		},
	})
}

func (m *delegateNetworkManager) ID() string {
	return constants.NetworkID.ID()
}

func (m *delegateNetworkManager) Init(ctx context.Context, opts *workflow.InitInfo) error {
	log.G(ctx).Errorf("Init doing")
	defer log.G(ctx).Errorf("Init end")

	err := m.tapPlugin.Init(ctx, opts)
	if err != nil {
		return err
	}

	err = m.db.Close()
	if err != nil {
		return err
	}
	time.Sleep(time.Second)
	if err := os.RemoveAll(filepath.Join(m.config.RootPath, "db")); err != nil {
		return fmt.Errorf("%v  RemoveAll failed:%v", filepath.Join(m.config.RootPath, "db"), err.Error())
	}

	if err = m.initDB(); err != nil {
		return err
	}
	m.allocationStore, err = networkstore.RecoverFromDB(m.db)
	if err != nil {
		return err
	}
	m.tapPlugin.SetAllocationStore(m.allocationStore)

	return nil
}

func (m *delegateNetworkManager) initDB() error {
	basePath := filepath.Join(m.config.RootPath, "db")
	if err := os.MkdirAll(path.Clean(basePath), os.ModeDir|0755); err != nil {
		return fmt.Errorf("init dir failed %s", err.Error())
	}
	var err error
	if m.db, err = utils.NewCubeStoreExt(basePath, "meta.db", 10, nil); err != nil {
		return err
	}

	registerBucket.CubeStore = m.db
	multimeta.RegisterBucket(&registerBucket)
	return nil
}

func (m *delegateNetworkManager) Create(ctx context.Context, opts *workflow.CreateContext) (err error) {
	defer func() {
		if err != nil {
			log.G(ctx).Errorf("Create,fail:%v", err.Error())
		}
	}()

	if opts.IsCreateSnapshot() {
		alloc, err := m.allocationStore.Get(opts.GetSandboxID())
		if err == nil && alloc.SandboxID == opts.GetSandboxID() {
			return ret.Err(errorcode.ErrorCode_PreConditionFailed, "already exists")
		}
	}

	switch opts.ReqInfo.NetworkType {
	case cubebox.NetworkType_tap.String():
		err = m.tapPlugin.Create(ctx, opts)
	default:
		return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "invalid network type %s", opts.ReqInfo.NetworkType)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return ret.WrapWithDefaultError(err, errorcode.ErrorCode_CreateNetworkFailed)
	}
	net := networkstore.NetworkAllocation{
		SandboxID:   opts.SandboxID,
		AppID:       0,
		NetworkType: opts.ReqInfo.NetworkType,
		Metadata:    opts.NetworkInfo,
		Timestamp:   time.Now().Unix(),
	}

	m.allocationStore.Add(net)
	if err := m.allocationStore.Sync(net.SandboxID); err != nil {
		return ret.Errorf(errorcode.ErrorCode_UpdateLocalMetaDataFailed, "%s", err.Error())
	}

	return nil
}

func (m *delegateNetworkManager) Destroy(ctx context.Context, opts *workflow.DestroyContext) (err error) {
	defer func() {
		if err != nil {
			log.G(ctx).Errorf("Destroy,fail:%v", err.Error())
		}
	}()

	var alloc networkstore.NetworkAllocation
	allocFound := false
	if m.allocationStore != nil {
		var getErr error
		alloc, getErr = m.allocationStore.Get(opts.SandboxID)
		if getErr != nil {
			if !errors.Is(getErr, utils.ErrorKeyNotFound) {
				return getErr
			}
			log.G(ctx).Warnf("network allocation metadata %s not found; destroying runtime state anyway", opts.SandboxID)
		} else {
			allocFound = true
		}
	}

	if allocFound {
		switch alloc.NetworkType {
		case cubebox.NetworkType_tap.String():
			err = m.tapPlugin.Destroy(ctx, opts)
		default:
			return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "invalid network type %s", alloc.NetworkType)
		}
	} else {
		err = m.tapPlugin.Destroy(ctx, opts)
	}
	if err != nil {
		return ret.Errorf(errorcode.ErrorCode_DestroyNetworkFailed, "%s", err.Error())
	}

	if allocFound {
		if err := m.allocationStore.DeleteSync(opts.SandboxID); err != nil {
			return ret.Errorf(errorcode.ErrorCode_UpdateLocalMetaDataFailed, "%s", err.Error())
		}
	}

	return nil
}

func (m *delegateNetworkManager) CleanUp(ctx context.Context, opts *workflow.CleanContext) (err error) {
	if opts == nil {
		return nil
	}
	defer func() {
		if err != nil {
			log.G(ctx).Errorf("CleanUp,fail:%v", err.Error())
		}
	}()
	requestID := ""
	rt := CubeLog.GetTraceInfo(ctx)
	if rt != nil {
		requestID = rt.RequestID
	}
	if requestID == "" {
		requestID = uuid.New().String()
		rt = rt.DeepCopy()
		rt.RequestID = requestID
		ctx = CubeLog.WithRequestTrace(ctx, rt)
	}
	log.G(ctx).Errorf("network CleanUp")
	destroyOpt := &workflow.DestroyContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: opts.SandboxID,
		},
		DestroyInfo: &cubebox.DestroyCubeSandboxRequest{
			SandboxID:   opts.SandboxID,
			RequestID:   requestID,
			Annotations: map[string]string{},
		},
	}
	if err := m.Destroy(ctx, destroyOpt); err != nil {

		log.G(ctx).WithField("plugin", "delegateNetworkManager").Errorf("CleanUp fail:%v", err)
		return err
	}
	return nil
}

func (m *delegateNetworkManager) Close() error {
	return nil
}

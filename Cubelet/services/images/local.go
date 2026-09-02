// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package images

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/platforms"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/server/images"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/pmem"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	oldimagestore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type Config struct {
	RootPath    string `toml:"root_path"`
	StatePath   string `toml:"state_path"`
	RuntimeType string `toml:"runtime_type"`

	PullDeadlineStr string `toml:"pull_dead_line"`
	pullDeadline    time.Duration

	DiscardUnpackedLayers bool `toml:"discard_unpacked_layers"`

	CubeToolBaseDir string `toml:"cubetool_base_dir"`
}

type local struct {
	Config   *Config
	client   *containerd.Client
	criImage *images.CubeImageService
}

var imgSrv *local

var defaultPullDeadline = 60 * time.Second

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.InternalPlugin,
		ID:     constants.ImagesID.ID(),
		Config: &Config{},
		Requires: []plugin.Type{
			plugins.CRIServicePlugin,
			plugins.EventPlugin,
			plugins.ServicePlugin,
			plugins.MetadataPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {

			config := ic.Config.(*Config)
			if config.RootPath == "" {
				config.RootPath = ic.Properties[plugins.PropertyRootDir]
			}
			if config.StatePath == "" {
				config.StatePath = ic.Properties[plugins.PropertyStateDir]
			}

			if config.CubeToolBaseDir == "" {
				config.CubeToolBaseDir = "/usr/local/services/cubetoolbox"
			}
			t, err := time.ParseDuration(config.PullDeadlineStr)
			if err != nil || t == 0 {
				config.pullDeadline = defaultPullDeadline
			} else {
				config.pullDeadline = t
			}

			CubeLog.Infof("%v init config:%+v",
				fmt.Sprintf("%v.%v", constants.InternalPlugin, constants.ImagesID), config)

			client, err := containerd.New(
				"",
				containerd.WithDefaultPlatform(platforms.Default()),
				containerd.WithInMemoryServices(ic),
			)
			if err != nil {
				return nil, fmt.Errorf("init containerd connect failed.%s", err)
			}

			obj, err := ic.GetByID(plugins.CRIServicePlugin, "images")
			if err != nil {
				return nil, fmt.Errorf("failed to get cri images service: %w", err)
			}

			imgSrv = &local{
				Config:   config,
				client:   client,
				criImage: obj.(*images.CubeImageService),
			}

			db, err := imgSrv.initDb(config.StatePath)
			if err != nil {
				return nil, fmt.Errorf("cfsImageManager init db failed:%s", err)
			}

			uidFileDir := filepath.Join(config.RootPath, "rootfs")
			err = os.MkdirAll(uidFileDir, os.ModeDir|0755)
			if err != nil {
				return nil, fmt.Errorf("image service init uidfiles failed:%s", err)
			}
			ois := oldimagestore.NewStore(client, config.RuntimeType, db, oldimagestore.WithUidFileDir(uidFileDir))
			_ = ois

			pmem.Init(config.CubeToolBaseDir)
			err = imgSrv.recover()
			if err != nil {
				return nil, fmt.Errorf("recover images failed: %w", err)
			}
			return imgSrv, nil
		},
	})
}

func (l *local) ID() string {
	return constants.ImagesID.ID()
}

func (l *local) Init(ctx context.Context, opts *workflow.InitInfo) error {
	os.RemoveAll(l.Config.StatePath)
	return l.criImage.Cleanup(ctx)
}

func (l *local) initDb(statePath string) (*utils.CubeStore, error) {
	basePath := filepath.Join(statePath, "db")
	if err := os.MkdirAll(path.Clean(basePath), os.ModeDir|0755); err != nil {
		return nil, fmt.Errorf("init dir failed %s", err.Error())
	}
	return utils.NewCubeStoreExt(basePath, "meta.db", 10, nil)
}

func (l *local) recover() error {
	CubeLog.Infof("start recover images")
	ctx := context.Background()
	nslist, err := l.client.NamespaceService().List(ctx)
	if err != nil {
		CubeLog.Warnf("loading namespaces fail:%v", err)
		return err
	}
	for _, ns := range nslist {
		err := l.criImage.CheckImages(namespaces.WithNamespace(ctx, ns))
		if err != nil {
			return fmt.Errorf("check images: %w", err)
		}
	}

	CubeLog.Infof("recover images success")
	return nil
}

func (l *local) Create(ctx context.Context, opts *workflow.CreateContext) error {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.CreateContext nil")
	}

	realReq := opts.ReqInfo
	for _, c := range realReq.Containers {
		ctx = constants.WithImageSpec(ctx, c.GetImage())
		i, err := l.criImage.EnsureImage(ctx, c.GetImage().GetImage(),
			c.GetImage().GetUsername(),
			c.GetImage().GetToken(),
			&runtime.PodSandboxConfig{})
		if err != nil {
			return fmt.Errorf("ensure image [%s] failed: %w", c.GetImage().GetImage(), err)
		}

		if i != nil && i.Labels() != nil && i.Labels()[constants.LabelImageUidFiles] != "" {

			uidFiles := i.Labels()[constants.LabelImageUidFiles]
			if _, err := os.Stat(uidFiles); err != nil {
				log.G(ctx).Warnf("image uidFiles [%s] not exist, retry update image", i.Name(), uidFiles)
				if err := l.criImage.UpdateImage(ctx, i.Name()); err != nil {
					log.G(ctx).Errorf("update image [%s] failed: %v", i.Name(), err)
					return err
				}
			}
		}
	}
	return nil
}

func (l *local) Destroy(ctx context.Context, opts *workflow.DestroyContext) error {
	if opts == nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "workflow.CreateContext nil")
	}
	return nil
}
func (l *local) CleanUp(ctx context.Context, opts *workflow.CleanContext) error {
	return nil
}

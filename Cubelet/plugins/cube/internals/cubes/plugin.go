// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubes

import (
	"context"
	"path"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

type cubeboxConfig struct {
	RootPath string `toml:"root_path"`
}

type DeleteOption struct {
	CubeboxID           string
	ContainerID         string
	SkipDeleteFlagCheck bool
}

type CubeboxAPI interface {
	Init(ctx context.Context) error

	Get(ctx context.Context, id string) (*cubeboxstore.CubeBox, error)
	FindContainerOfCubebox(ctx context.Context, ID string) (*cubeboxstore.Container, *cubeboxstore.CubeBox, error)
	List() []*cubeboxstore.CubeBox
	IsImageInUse(imageID string) (bool, error)

	Save(ctx context.Context, info *cubeboxstore.CubeBox, opts ...UpdateCubeboxOpt) error
	SyncByID(ctx context.Context, id string, opts ...UpdateCubeboxOpt) error

	Delete(ctx context.Context, opt *DeleteOption) error
}

type ContainerdClientSetter interface {
	SetContainerdClient(client *containerd.Client)
}

type RocoverCubebox interface {
	RecoverAllCubebox(ctx context.Context, afterRecover func(ctx context.Context, cb *cubeboxstore.CubeBox) error) error
}

func init() {
	registry.Register(&plugin.Registration{
		Type:     constants.CubeStorePlugin,
		ID:       constants.CubeboxID.ID(),
		Config:   defaultConfig(),
		Requires: []plugin.Type{},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			l := &local{
				eventChan: make(chan *CubeboxEvent, 10000),
			}
			l.config = ic.Config.(*cubeboxConfig)
			if l.config.RootPath == "" {
				l.config.RootPath = ic.Properties[plugins.PropertyStateDir]
			}

			err := l.initDb()
			if err != nil {
				return nil, err
			}
			l.cubeboxStore = cubeboxstore.NewStore(l.db)

			l.registerCDPDeleteHooks()
			go l.startListener()
			return l, nil
		},
	})
}

func defaultConfig() *cubeboxConfig {
	return &cubeboxConfig{
		RootPath: path.Join("/data/cubelet", "state", constants.InternalPlugin.String()+"."+constants.CubeboxID.ID()),
	}
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubes

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/multimetadb/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cdp"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/multimeta"
)

type local struct {
	config       *cubeboxConfig
	db           *utils.CubeStore
	cubeboxStore *cubeboxstore.Store

	client *containerd.Client

	listeners []CubeboxEventListener

	eventChan chan *CubeboxEvent
}

var (
	dbDir                        = "db"
	_     CubeboxAPI             = &local{}
	_     ContainerdClientSetter = &local{}

	registerBucket = multimeta.BucketDefineInternal{
		BucketDefine: &multimetadb.BucketDefine{
			Name:     cubeboxstore.DBBucketSandbox,
			DbName:   "cubebox",
			Describe: "cubebox metadata db",
		},
	}
)

func (l *local) initDb() error {
	basePath := filepath.Join(l.config.RootPath, dbDir)
	if err := os.MkdirAll(path.Clean(basePath), os.ModeDir|0755); err != nil {
		return fmt.Errorf("init dir %s failed %s", basePath, err.Error())
	}
	var err error
	if l.db, err = utils.NewCubeStoreExt(basePath, "meta.db", 10, nil); err != nil {
		return err
	}

	registerBucket.CubeStore = l.db
	multimeta.RegisterBucket(&registerBucket)
	return nil
}

func (l *local) Init(ctx context.Context) error {
	_ = l.db.Close()

	l.initDb()
	l.cubeboxStore = cubeboxstore.NewStore(l.db)

	return nil
}

func (l *local) Delete(ctx context.Context, opt *DeleteOption) error {
	if opt == nil {
		return nil
	}

	deleteOpt := &cdp.DeleteOption{
		ID:           opt.CubeboxID,
		ResourceType: cdp.ResourceDeleteProtectionTypeCubebox,
	}
	var err error

	if opt.ContainerID == "" {
		err = cdp.PreDelete(ctx, deleteOpt)
	}

	if err == nil {
		if opt.ContainerID != "" {

			_, err = l.client.ContainerService().Get(ctx, opt.ContainerID)
			if err != nil && !errdefs.IsNotFound(err) {
				return fmt.Errorf("cube delete failed to get container %q: %v", opt.ContainerID, err)
			} else if err == nil {
				return fmt.Errorf("failed to delete container %q: container is still exist", opt.ContainerID)
			}
			l.cubeboxStore.DeleteContainer(opt.ContainerID)
			err = l.cubeboxStore.Sync(opt.CubeboxID)
		} else {
			err = l.cubeboxStore.DeleteSync(opt.CubeboxID)
		}
		if err != nil {
			return err
		} else {
			err = cdp.PostDelete(ctx, deleteOpt)
		}
	}
	if err != nil {
		return fmt.Errorf("failed to delete cubebox %s: %w", opt.CubeboxID, err)
	}
	return nil
}

func (l *local) Get(ctx context.Context, id string) (*cubeboxstore.CubeBox, error) {
	sb, err := l.cubeboxStore.Get(id)
	if err != nil {
		return nil, err
	}
	if sb.SandboxID == "" {
		sb.SandboxID = sb.ID
	}
	return sb, nil
}

func (l *local) Save(ctx context.Context, cb *cubeboxstore.CubeBox, opts ...UpdateCubeboxOpt) error {
	opt := &CubeboxSaveOptions{}
	for _, o := range opts {
		o(opt)
	}
	l.cubeboxStore.Add(cb)

	if !opt.NoEvent {
		l.dispatch(&CubeboxEvent{
			EventType: CubeboxEventTypeUpdate,
			Cubebox:   cb,
		})
	}

	return l.cubeboxStore.Sync(cb.ID)
}

func (l *local) SyncByID(ctx context.Context, id string, opts ...UpdateCubeboxOpt) error {
	cb, err := l.Get(ctx, id)
	if err != nil {
		return err
	}
	opt := &CubeboxSaveOptions{}
	for _, o := range opts {
		o(opt)
	}
	if !opt.NoEvent {
		l.dispatch(&CubeboxEvent{
			EventType: CubeboxEventTypeUpdate,
			Cubebox:   cb,
		})
	}
	return l.cubeboxStore.Sync(id)
}

func (l *local) SetContainerdClient(client *containerd.Client) {
	l.client = client
}

func (l *local) FindContainerOfCubebox(ctx context.Context, id string) (cntr *cubeboxstore.Container, cb *cubeboxstore.CubeBox, err error) {
	cntr, _ = l.cubeboxStore.GetContainer(id)

	if cntr != nil && cntr.SandboxID != "" {
		cb, err = l.cubeboxStore.Get(cntr.SandboxID)
	} else {
		cb, err = l.cubeboxStore.Get(id)
	}
	if err != nil && !errdefs.IsNotFound(err) {
		return nil, nil, fmt.Errorf("failed to get cubebox from container %s event: %w", id, err)
	}

	if cntr == nil {
		cntr = cb.FirstContainer()
		if cntr == nil {
			return nil, nil, fmt.Errorf("failed to get first container of cubebox %s: %w", id, err)
		}
	}
	return cntr, cb, nil
}

func (l *local) List() []*cubeboxstore.CubeBox {
	return l.cubeboxStore.List()
}

// IsImageInUse reports whether the given image/artifact id is currently
// referenced by any sandbox tracked on this node. It mirrors the protection in
// cubeboxImageDeleteHook and is used by the ext4 artifact destroy path
// (DestroyImage) to refuse removing an OS image that a running sandbox depends
// on, without invoking the runtemplate catalog delete hook.
func (l *local) IsImageInUse(imageID string) (bool, error) {
	cbs, err := l.cubeboxStore.GetCubeboxByImageID(imageID)
	if err != nil {
		return false, err
	}
	return len(cbs) > 0, nil
}

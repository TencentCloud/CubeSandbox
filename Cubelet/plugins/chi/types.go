// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package chi

import (
	"context"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/snapshots"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubehost/v1"
	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/server/images"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

type CubeboxRuntimeUpdater interface {
	GetSandboxer() *cubeboxstore.CubeBox

	GetSnapShotter() (snapshots.Snapshotter, error)

	GetImageService() *cubeimages.CubeImageService

	AppendSharedImageFs(context.Context, *cubehost.HostImage) error

	RemoveLayerMounts(context.Context, []*cubehost.LayerMount) error
}

type ChiFactory interface {
	RunForwardCubeHostImage(ctx context.Context, updater CubeboxRuntimeUpdater, cubeBox *cubeboxstore.CubeBox, container containerd.Container) error
	CloseForwardCubeHostImage(ctx context.Context, sandboxID string) error

	ListAllVmImages(ctx context.Context) (map[string]sets.Set[string], error)
}

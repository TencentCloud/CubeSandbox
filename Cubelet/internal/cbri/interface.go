// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cbri

import (
	"context"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"

	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/server/images"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
)

type API interface {
	GetPassthroughMounts(ctx context.Context, flowOpts *workflow.CreateContext) ([]specs.Mount, error)
	CreateSandbox(context.Context, *workflow.CreateContext) ([]oci.SpecOpts, error)
	CreateContainer(context.Context, *cubeboxstore.CubeBox, *cubeboxstore.Container) ([]oci.SpecOpts, error)

	PostCreateContainer(context.Context, *cubeboxstore.CubeBox, *cubeboxstore.Container) error
	DestroySandbox(context.Context, string) error
}

type APIInit interface {
	SetCubeRuntimeImplementation(CubeRuntimeImplementation)
}
type APIManager interface {
	APIInit
	API
}

type CubeRuntimeImplementation interface {
	CubeboxStore() cubes.CubeboxAPI
	GetImageService() *cubeimages.CubeImageService
	GetSnapshotter(string) (snapshots.Snapshotter, error)
}

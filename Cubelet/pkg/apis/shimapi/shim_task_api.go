// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package shimapi

import (
	"context"
	"fmt"
	"path"

	"github.com/containerd/containerd/v2/client"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

type CubeShimAPI interface {
	CubeShimDeviceAPI
	CubeVirtioFSAPI
	ChDiskAPI
}

type cubeShimControl struct {
	cubebox *cubebox.CubeBox
	task    client.Task
}

func NewCubeShimClient(ctx context.Context, cubebox *cubebox.CubeBox) (CubeShimAPI, error) {
	task, err := cubebox.FirstContainer().Container.Task(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get task for container %s: %v", cubebox.FirstContainer().Container.ID(), err)
	}

	return &cubeShimControl{
		cubebox: cubebox,
		task:    task,
	}, nil
}

func (csc *cubeShimControl) getChSockPath() string {
	if csc.cubebox == nil {
		return ""
	}
	return path.Join("/run/vc/vm/", csc.cubebox.ID, "chapi")
}

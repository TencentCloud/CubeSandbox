// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package sandbox

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/containerd/api/types"

	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/core/events/exchange"
	v2 "github.com/containerd/containerd/v2/core/runtime/v2"
	"github.com/containerd/containerd/v2/core/sandbox"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.SandboxControllerPlugin,
		ID:   "cube",
		Requires: []plugin.Type{
			plugins.ShimPlugin,
			plugins.EventPlugin,
		},
		Config: defaultSandboxConfig(),
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			shimPlugin, err := ic.GetSingle(plugins.ShimPlugin)
			if err != nil {
				return nil, err
			}

			exchangePlugin, err := ic.GetByID(plugins.EventPlugin, "exchange")
			if err != nil {
				return nil, err
			}

			var (
				shims     = shimPlugin.(*v2.ShimManager)
				publisher = exchangePlugin.(*exchange.Exchange)
			)
			config := ic.Config.(*sandboxConfig)
			state := config.StatePath
			root := config.RootPath
			for _, d := range []string{root, state} {
				if err := os.MkdirAll(d, 0700); err != nil {
					return nil, err
				}

				if err := os.Chmod(d, 0o700); err != nil {
					return nil, err
				}
			}

			if err := shims.LoadExistingShims(ic.Context, state, root); err != nil {
				return nil, fmt.Errorf("failed to load existing shim sandboxes, %v", err)
			}

			c := &controllerLocal{
				root:      root,
				state:     state,
				shims:     shims,
				publisher: publisher,
			}
			return c, nil
		},
	})
}

type sandboxConfig struct {
	RootPath  string `toml:"root_path"`
	StatePath string `toml:"state_path"`
}

func defaultSandboxConfig() *sandboxConfig {
	return &sandboxConfig{
		RootPath:  "/data/cubelet/root/io.containerd.runtime.v2/task",
		StatePath: "/data/cubelet/state/io.containerd.runtime.v2/task",
	}
}

type controllerLocal struct {
	root      string
	state     string
	shims     *v2.ShimManager
	publisher events.Publisher
}

type taskStatsService interface {
	Stats(context.Context, *task.StatsRequest) (*task.StatsResponse, error)
}

var _ sandbox.Controller = (*controllerLocal)(nil)

func (c *controllerLocal) Create(ctx context.Context, info sandbox.Sandbox, opts ...sandbox.CreateOpt) (retErr error) {
	return nil
}

func (c *controllerLocal) Start(ctx context.Context, sandboxID string) (sandbox.ControllerInstance, error) {
	return sandbox.ControllerInstance{}, nil
}

func (c *controllerLocal) Platform(ctx context.Context, sandboxID string) (imagespec.Platform, error) {
	var platform imagespec.Platform
	return platform, nil
}

func (c *controllerLocal) Stop(ctx context.Context, sandboxID string, opts ...sandbox.StopOpt) error {
	return nil
}

func (c *controllerLocal) Shutdown(ctx context.Context, sandboxID string) error {
	return nil
}

func (c *controllerLocal) Wait(ctx context.Context, sandboxID string) (sandbox.ExitStatus, error) {
	svc, err := c.getSandbox(ctx, sandboxID)
	if errdefs.IsNotFound(err) {
		return sandbox.ExitStatus{
			ExitedAt:   time.Now(),
			ExitStatus: 1,
		}, nil
	}
	resp, err := svc.Wait(ctx, &task.WaitRequest{
		ID: sandboxID,
	})
	if err != nil {
		return sandbox.ExitStatus{
			ExitedAt:   resp.GetExitedAt().AsTime(),
			ExitStatus: resp.GetExitStatus(),
		}, err
	}
	return sandbox.ExitStatus{
		ExitedAt:   resp.GetExitedAt().AsTime(),
		ExitStatus: resp.GetExitStatus(),
	}, nil
}

func (c *controllerLocal) Status(ctx context.Context, sandboxID string, verbose bool) (sandbox.ControllerStatus, error) {
	svc, err := c.getSandbox(ctx, sandboxID)
	if errdefs.IsNotFound(err) {
		return sandbox.ControllerStatus{
			SandboxID: sandboxID,
			ExitedAt:  time.Now(),
		}, nil
	}
	if err != nil {
		return sandbox.ControllerStatus{}, err
	}

	resp, err := svc.State(ctx, &task.StateRequest{
		ID: sandboxID,
	})
	if err != nil {
		return sandbox.ControllerStatus{}, fmt.Errorf("failed to query sandbox %s status: %w", sandboxID, err)
	}

	shim, err := c.shims.Get(ctx, sandboxID)
	if err != nil {
		return sandbox.ControllerStatus{}, fmt.Errorf("unable to find sandbox %q", sandboxID)
	}
	address, version := shim.Endpoint()

	return sandbox.ControllerStatus{
		SandboxID: sandboxID,
		Pid:       resp.GetPid(),
		State:     resp.GetStatus().String(),
		ExitedAt:  resp.GetExitedAt().AsTime(),
		Address:   address,
		Version:   uint32(version),
	}, nil
}

func (c *controllerLocal) Metrics(ctx context.Context, sandboxID string) (*types.Metric, error) {
	return c.taskMetrics(ctx, sandboxID, sandboxID)
}

func (c *controllerLocal) taskMetrics(ctx context.Context, sandboxID, containerID string) (*types.Metric, error) {
	if sandboxID == "" {
		return nil, fmt.Errorf("sandbox ID is required")
	}
	if containerID == "" {
		return nil, fmt.Errorf("container ID is required")
	}
	svc, err := c.getSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	return metricFromTaskStats(ctx, svc, sandboxID, containerID)
}

func metricFromTaskStats(ctx context.Context, svc taskStatsService, sandboxID, containerID string) (*types.Metric, error) {
	resp, err := svc.Stats(ctx, &task.StatsRequest{ID: containerID})
	if err != nil {
		return nil, fmt.Errorf(
			"failed to query workload metrics for %s/%s: %w",
			sandboxID,
			containerID,
			errgrpc.ToNative(err),
		)
	}
	raw := resp.GetStats()
	if raw == nil {
		return nil, fmt.Errorf("workload metrics for %s/%s are empty", sandboxID, containerID)
	}
	return &types.Metric{
		Timestamp: timestamppb.Now(),
		ID:        containerID,
		Data:      raw,
	}, nil
}

func (c *controllerLocal) Update(
	ctx context.Context,
	sandboxID string,
	sandbox sandbox.Sandbox,
	fields ...string) error {
	return nil
}

func (c *controllerLocal) getSandbox(ctx context.Context, id string) (task.TaskService, error) {
	shim, err := c.shims.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	taskClient, ok := shim.Client().(*ttrpc.Client)
	if !ok {
		return nil, fmt.Errorf("failed to get task client")
	}
	return task.NewTaskClient(taskClient), nil
}

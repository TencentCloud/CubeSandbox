// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"path/filepath"

	"github.com/containerd/containerd/v2/pkg/namespaces"

	cubeboxapi "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/services/cubebox/terminalcore"
)

func newTerminalRuntimeAdapter(manager *local) (*terminalcore.ContainerdAdapter, error) {
	resolver := func(ctx context.Context, sandboxID, containerID string) (*terminalcore.ContainerdTarget, error) {
		sandbox, err := manager.cubeboxManger.Get(ctx, sandboxID)
		if err != nil {
			return nil, terminalcore.WrapError(terminalcore.CodeTargetNotFound, err)
		}
		if sandbox.GetStatus().Get().State() != cubeboxapi.ContainerState_CONTAINER_RUNNING {
			return nil, terminalcore.Errorf(terminalcore.CodeTargetNotRunning, "sandbox is not running")
		}

		namespace := sandbox.Namespace
		if namespace == "" {
			namespace = namespaces.Default
		}
		ctx = namespaces.WithNamespace(ctx, namespace)
		logicalContainerID := containerID
		if logicalContainerID == "" {
			logicalContainerID = sandbox.FirstContainerName
			if logicalContainerID == "" {
				logicalContainerID = sandbox.ID
			}
		}
		containerRecord, err := sandbox.Get(logicalContainerID)
		if err != nil {
			return nil, terminalcore.WrapError(terminalcore.CodeTargetNotFound, err)
		}
		if containerRecord.Status != nil && containerRecord.Status.Get().State() != cubeboxapi.ContainerState_CONTAINER_RUNNING {
			return nil, terminalcore.Errorf(terminalcore.CodeTargetNotRunning, "container is not running")
		}

		runtimeContainer := containerRecord.Container
		if runtimeContainer == nil {
			runtimeContainer, err = manager.client.LoadContainer(ctx, logicalContainerID)
			if err != nil {
				return nil, terminalcore.WrapError(terminalcore.CodeTargetNotFound, err)
			}
		}
		return &terminalcore.ContainerdTarget{
			Meta: terminalcore.TargetMetadata{
				SandboxID:          sandbox.ID,
				ContainerID:        logicalContainerID,
				Namespace:          namespace,
				RuntimeContainerID: runtimeContainer.ID(),
			},
			Container: runtimeContainer,
		}, nil
	}

	return terminalcore.NewContainerdAdapter(
		manager.client,
		resolver,
		filepath.Join(manager.config.StatePath, "terminal-fifo"),
	)
}

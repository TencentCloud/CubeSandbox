// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"fmt"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/sandbox"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: constants.InternalPlugin,
		ID:   "cube-sandbox-store",
		Requires: []plugin.Type{
			plugins.ServicePlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			client, err := containerd.New(
				"",
				containerd.WithDefaultPlatform(platforms.Default()),
				containerd.WithInMemoryServices(ic),
			)
			if err != nil {
				return nil, fmt.Errorf("init containerd connect failed.%s", err)
			}

			return &cubeSandboxStorePlugin{client: client}, nil
		},
	})
}

type cubeSandboxStorePlugin struct {
	client *containerd.Client
}

func (p *cubeSandboxStorePlugin) CleanUp(ctx context.Context, opts *workflow.CleanContext) error {
	return p.Destroy(ctx, &workflow.DestroyContext{
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: opts.SandboxID,
		},
	})
}

func (p *cubeSandboxStorePlugin) ID() string {
	return "cube-sandbox-store"
}

func (p *cubeSandboxStorePlugin) Init(ctx context.Context, opts *workflow.InitInfo) error {
	nses, err := p.client.NamespaceService().List(ctx)
	if err != nil {
		return fmt.Errorf("list namespaces failed.%s", err)
	}

	for _, ns := range nses {
		tempCtx := namespaces.WithNamespace(ctx, ns)
		sbs, err := p.client.SandboxStore().List(tempCtx)
		if err != nil {
			return fmt.Errorf("list sandboxes failed.%s", err)
		}
		for _, sb := range sbs {
			err = p.client.SandboxStore().Delete(tempCtx, sb.ID)
			if err != nil {
				log.G(tempCtx).Errorf("delete sandbox failed.%s", err)
			} else {
				log.G(tempCtx).Infof("delete sandbox %s success.", sb.ID)
			}
		}
	}
	return nil
}

func (p *cubeSandboxStorePlugin) Create(ctx context.Context, opts *workflow.CreateContext) error {
	_, err := p.client.SandboxStore().Create(ctx,
		sandbox.Sandbox{
			ID:        opts.SandboxID,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
			Labels:    opts.ReqInfo.Labels,
			Sandboxer: "cube",
		})
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			log.G(ctx).Errorf("sandbox %s already exists.", opts.SandboxID)
			return ret.Errorf(errorcode.ErrorCode_PreConditionFailed, "%s", err.Error())
		}
		return fmt.Errorf("create sandbox failed.%w", err)
	}
	log.G(ctx).Debugf("create sandbox %s success.", opts.SandboxID)
	return nil
}

func (p *cubeSandboxStorePlugin) Destroy(ctx context.Context, opts *workflow.DestroyContext) error {
	err := p.client.SandboxStore().Delete(ctx, opts.SandboxID)
	if err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	log.G(ctx).Debugf("delete sandbox %s success.", opts.SandboxID)
	return nil
}

var _ workflow.Flow = (*cubeSandboxStorePlugin)(nil)

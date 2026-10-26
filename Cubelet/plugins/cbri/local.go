// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cbriplugin

import (
	"context"
	"fmt"
	"log"

	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cbri"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: constants.PluginCBRIManager,
		ID:   constants.PluginManager,
		Requires: []plugin.Type{
			constants.PluginCBRI,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			subPlugins := make(map[string]cbri.API)
			plgs, err := ic.GetByType(constants.PluginCBRI)
			if err != nil {
				return nil, fmt.Errorf("failed to get cbri plugins: %w", err)
			}
			for k, p := range plgs {
				api, ok := p.(cbri.API)
				if !ok {
					log.Fatalf("plugin [%s] is not a cbri plugin", k)
				}
				subPlugins[k] = api
			}

			return &local{
				items: subPlugins,
			}, nil
		},
	})
}

type local struct {
	items map[string]cbri.API
	cri   cbri.CubeRuntimeImplementation
}

func (l *local) CreateContainer(ctx context.Context, cubebox *cubeboxstore.CubeBox, c *cubeboxstore.Container) ([]oci.SpecOpts, error) {
	var specs []oci.SpecOpts
	for key, item := range l.items {
		spec, err := item.CreateContainer(ctx, cubebox, c)
		if err != nil {
			return nil, fmt.Errorf("failed to run plugin %s CreateContainer: %w", key, err)
		}
		if len(spec) > 0 {
			specs = append(specs, spec...)
		}
	}
	return specs, nil
}

func (l *local) CreateSandbox(ctx context.Context, create *workflow.CreateContext) ([]oci.SpecOpts, error) {
	var specs []oci.SpecOpts
	for key, item := range l.items {
		spec, err := item.CreateSandbox(ctx, create)
		if err != nil {
			return nil, fmt.Errorf("failed to run plugin %s CreateSandbox: %w", key, err)
		}
		if len(spec) > 0 {
			specs = append(specs, spec...)
		}
	}
	return specs, nil
}

func (l *local) GetPassthroughMounts(ctx context.Context, flowOpts *workflow.CreateContext) ([]specs.Mount, error) {
	var specs []specs.Mount
	for key, item := range l.items {
		spec, err := item.GetPassthroughMounts(ctx, flowOpts)
		if err != nil {
			return nil, fmt.Errorf("failed to run plugin %s GetPassthroughMounts: %w", key, err)
		}
		if len(spec) > 0 {
			specs = append(specs, spec...)
		}
	}
	return specs, nil
}

func (l *local) PostCreateContainer(ctx context.Context, cubebox *cubeboxstore.CubeBox, c *cubeboxstore.Container) error {
	for key, item := range l.items {
		err := item.PostCreateContainer(ctx, cubebox, c)
		if err != nil {
			return fmt.Errorf("failed to run plugin %s PostCreateContainer: %w", key, err)
		}
	}
	return nil
}

func (l *local) SetCubeRuntimeImplementation(cri cbri.CubeRuntimeImplementation) {
	l.cri = cri
	for _, item := range l.items {
		if m, ok := item.(cbri.APIInit); ok {
			m.SetCubeRuntimeImplementation(cri)
		}
	}
}

func (l *local) DestroySandbox(ctx context.Context, sandboxID string) error {
	for key, item := range l.items {
		err := item.DestroySandbox(ctx, sandboxID)
		if err != nil {
			return fmt.Errorf("failed to run plugin %s DestroySandbox: %w", key, err)
		}
	}
	return nil
}

var _ cbri.APIManager = &local{}

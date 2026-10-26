// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package transfer

import (
	"context"
	"fmt"
	"reflect"

	"github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: constants.CubeTransferManager,
		ID:   constants.PluginManager,
		Requires: []plugin.Type{
			plugins.TransferPlugin,
			plugins.StreamingPlugin,
		},
		InitFn: newService,
	})
}

type service struct {
	transferrers []transfer.Transferrer
}

func newService(ic *plugin.InitContext) (interface{}, error) {
	sps, err := ic.GetByType(plugins.TransferPlugin)
	if err != nil {
		return nil, err
	}

	t := make([]transfer.Transferrer, 0, len(sps))
	for _, p := range sps {
		t = append(t, p.(transfer.Transferrer))
	}
	return &service{
		transferrers: t,
	}, nil
}

var _ transfer.Transferrer = &service{}

func (s *service) Transfer(ctx context.Context, source interface{}, destination interface{}, opts ...transfer.Opt) error {

	for _, t := range s.transferrers {
		if err := t.Transfer(ctx, source, destination, opts...); err == nil {
			return nil
		} else if !errdefs.IsNotImplemented(err) {
			return err
		}
	}
	return fmt.Errorf("method Transfer not implemented for %s to %s", reflect.TypeOf(source).String(), reflect.TypeOf(destination).String())
}

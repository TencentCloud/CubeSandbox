// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cgroup

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func GenOpt(ctx context.Context, c *cubebox.ContainerConfig) ([]oci.SpecOpts, error) {
	var opts []oci.SpecOpts
	if c.GetResources() == nil {
		return opts, nil
	}
	log.G(ctx).Debugf("container resource: %+v", c.GetResources())

	memStr := c.GetResources().GetMemLimit()
	if memStr == "" {
		memStr = c.GetResources().GetMem()
	}

	if memStr != "" {
		memQ, err := resource.ParseQuantity(memStr)
		if err != nil {
			return opts, fmt.Errorf("resource memory limit %s: %v", memStr, err)
		}
		opts = append(opts, oci.WithMemoryLimit(uint64(memQ.Value())))
	}

	return opts, nil
}

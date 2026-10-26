// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package capability

import (
	"context"

	"github.com/containerd/containerd/v2/pkg/cap"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	customopts "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/opts"
)

func GenOpt(ctx context.Context, c *cubebox.ContainerConfig) []oci.SpecOpts {
	var opts []oci.SpecOpts
	allCaps, err := cap.Current()
	if err != nil {
		return opts
	}
	if c.SecurityContext == nil || c.SecurityContext.GetCapabilities() == nil {
		return opts
	}
	opts = append(opts, customopts.WithCapabilities(toLinuxContainerSecurityContext(c.SecurityContext.GetCapabilities()), allCaps))
	return opts
}

func toLinuxContainerSecurityContext(cS *cubebox.Capability) *runtime.LinuxContainerSecurityContext {
	sc := &runtime.LinuxContainerSecurityContext{
		Capabilities: &runtime.Capability{
			DropCapabilities: cS.DropCapabilities,
			AddCapabilities:  cS.AddCapabilities,
		},
	}
	return sc
}

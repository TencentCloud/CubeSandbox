// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet/versioninfo"

type guestEnvironmentVersions struct {
	GuestImage string
	Agent      string
	Kernel     string
}

func collectGuestEnvironmentVersions() guestEnvironmentVersions {
	collector := versioninfo.NewCollector("")
	versions := collector.Collect()
	out := guestEnvironmentVersions{}
	for _, item := range versions {
		switch item.Component {
		case versioninfo.ComponentGuestImage:
			out.GuestImage = item.Version
		case versioninfo.ComponentCubeAgent:
			out.Agent = item.Version
		case versioninfo.ComponentKernel:
			out.Kernel = item.Version
		}
	}
	return out
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package provider

import (
	"github.com/containerd/containerd/v2/pkg/oci"
	networktypes "github.com/tencentcloud/CubeSandbox/Cubelet/network/types"
)

type NetworkProvider interface {
	SandboxIP() string
	AllocatedPorts() []networktypes.PortMapping
	OCISpecOpts() oci.SpecOpts
	GetPersistMetadata() []byte
}

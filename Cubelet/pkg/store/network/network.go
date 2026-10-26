// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

import "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow/provider"

type NetworkAllocation struct {
	SandboxID          string
	AppID              int64
	NetworkType        string
	Metadata           provider.NetworkProvider `json:"-"`
	PersistentMetadata []byte
	Timestamp          int64
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
)

const (
	columnContainer  = "CONTAINER"
	columnImage      = "IMAGE"
	columnImageID    = "IMAGE ID"
	columnCreated    = "CREATED"
	columnState      = "STATE"
	columnName       = "NAME"
	columnAttempt    = "ATTEMPT"
	columnPodName    = "POD"
	columnPodID      = "POD ID"
	columnPodRuntime = "RUNTIME"
	columnNamespace  = "NAMESPACE"
	columnSize       = "SIZE"
	columnMedia      = "MEDIA"
	columnTag        = "TAG"
	columnPinned     = "PINNED"
	columnDigest     = "DIGEST"
	columnMemory     = "MEM"
	columnInodes     = "INODES"
	columnSwap       = "SWAP"
	columnDisk       = "DISK"
	columnCPU        = "CPU %"
	columnKey        = "KEY"
	columnValue      = "VALUE"
)

func newDefaultTableDisplay() *commands.Display {
	return commands.NewDefaultTableDisplay()
}

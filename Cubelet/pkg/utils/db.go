// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"github.com/containerd/containerd/v2/pkg/timeout"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	bolt "go.etcd.io/bbolt"
)

func MakeBoltDBOption() *bolt.Options {
	opt := &bolt.Options{
		NoGrowSync: true,

		NoFreelistSync: true,

		FreelistType: bolt.FreelistMapType,

		NoSync:  true,
		Timeout: timeout.Get(constants.BoltOpenTimeout),
	}
	return opt
}

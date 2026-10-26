// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build !windows

package transfer

import (
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/platforms"
)

func defaultUnpackConfig() []unpackConfiguration {
	return []unpackConfiguration{
		{
			Platform:    platforms.Format(platforms.DefaultSpec()),
			Snapshotter: defaults.DefaultSnapshotter,
		},
	}
}

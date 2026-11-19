// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build !linux

package checkpoint

import "os"

func dataExtents(_ *os.File, size int64) ([][2]int64, error) {
	if size == 0 {
		return nil, nil
	}
	return [][2]int64{{0, size}}, nil
}

func dropCache(*os.File, int64, int64) {}

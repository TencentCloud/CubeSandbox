// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build !linux

package jfsdiff

import "context"

func warmFile(context.Context, string, int, float64) (WarmStats, error) {
	return WarmStats{}, errUnsupported
}

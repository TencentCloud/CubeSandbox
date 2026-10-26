// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build !windows
// +build !windows

package cubelet

func getOSSpecificLabels() (map[string]string, error) {
	return nil, nil
}

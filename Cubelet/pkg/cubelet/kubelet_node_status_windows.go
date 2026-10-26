// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build windows
// +build windows

package cubelet

import (
	"errors"
)

func getOSSpecificLabels() (map[string]string, error) {
	return nil, errors.New("not implemented")
}

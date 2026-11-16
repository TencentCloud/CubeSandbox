// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build !linux

package storage

import "errors"

func mountBDI(string) (string, error) {
	return "", errors.New("BDI lookup requires linux")
}

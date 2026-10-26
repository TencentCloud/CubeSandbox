// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import "strings"

func IsNotFound(err error) bool {
	return ContainsError(err, "not found")
}

func ContainsError(err error, keys string) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), keys)
}

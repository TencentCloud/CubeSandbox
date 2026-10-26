// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubes

import (
	"strings"

	"github.com/containerd/errdefs"
)

func IsNotFoundContainerError(err error) bool {
	if err == nil {
		return false
	}
	if errdefs.IsNotFound(err) ||
		strings.Contains(err.Error(), "not found") ||
		strings.Contains(err.Error(), "not exist") {
		return true
	}
	return false
}

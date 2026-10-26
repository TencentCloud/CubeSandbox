// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build openbsd

package mount

import "github.com/containerd/errdefs"

func UnmountRecursive(mount string, flags int) error {
	return errdefs.ErrNotImplemented
}

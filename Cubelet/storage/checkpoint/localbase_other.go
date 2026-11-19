// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build !linux

package checkpoint

import (
	"errors"
	"os"
)

func cloneInto(*os.File, *os.File) error {
	return errors.New("reflink is only available on Linux")
}

func punchHole(*os.File, int64, int64) error {
	return errors.New("hole punching is only available on Linux")
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

//go:build !linux

package checkpoint

import (
	"errors"
	"os"
)

func physicalExtents(*os.File) ([]extent, error) {
	return nil, errors.New("FIEMAP is only available on Linux")
}

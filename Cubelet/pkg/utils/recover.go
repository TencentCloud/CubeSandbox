// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"fmt"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func Recover() error {
	if err := recover(); err != nil {
		CubeLog.Fatalf("panic: %+v, stack: %s", err, recov.DumpStacktrace(3, err))
		return fmt.Errorf("%v", err)
	}
	return nil
}

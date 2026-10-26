// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"

	"github.com/tencentcloud/CubeSandbox/CubeOps/cmd/cubeopscli/app"
)

func main() {
	if err := app.Run(); err != nil {
		os.Exit(1)
	}
}

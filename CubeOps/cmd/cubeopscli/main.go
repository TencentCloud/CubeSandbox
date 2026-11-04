// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	"github.com/tencentcloud/CubeSandbox/CubeOps/cmd/cubeopscli/app"
)

func main() {
	if err := app.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "cubeopscli run fail: %s\n", err)
		os.Exit(1)
	}
}

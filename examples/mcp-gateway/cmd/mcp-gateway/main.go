// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"

	"github.com/tencentcloud/CubeSandbox/examples/mcp-gateway/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr, cli.DefaultPaths()))
}

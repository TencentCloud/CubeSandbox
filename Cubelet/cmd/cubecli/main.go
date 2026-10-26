// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"fmt"
	"os"

	"github.com/urfave/cli/v2"

	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/app"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubemnt"
)

var pluginCmds = []*cli.Command{}

func main() {
	os.Setenv("CONTAINERD_SUPPRESS_DEPRECATION_WARNINGS", "true")
	app := app.New()
	app.Commands = append(app.Commands, pluginCmds...)
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "cubecli run fail: %s\n", err)
		os.Exit(1)
	}
}

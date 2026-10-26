// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"fmt"
	"os"

	"github.com/urfave/cli"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/cmd/cubemastercli/app"
)

var pluginCmds = []cli.Command{}

func main() {
	app := app.New()
	app.Commands = append(app.Commands, pluginCmds...)
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "cubemastercli run fail: %s\n", err)
		os.Exit(1)
	}
}

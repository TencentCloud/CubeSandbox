// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"os"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/cmd/cubeopscli/commands/node"
	"github.com/tencentcloud/CubeSandbox/CubeOps/cmd/cubeopscli/commands/version"
	pkgv "github.com/tencentcloud/CubeSandbox/CubeOps/internal/version"
	"github.com/urfave/cli"
)

func init() {
	cli.VersionPrinter = func(c *cli.Context) {
		fmt.Println(pkgv.VersionString("cubeopscli"))
	}
}

func New() *cli.App {
	app := cli.NewApp()
	app.Name = "cubeopscli"
	app.Usage = "CubeOps command line tool"
	app.Version = pkgv.ShowVersion()
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "a, address",
			Value: "127.0.0.1",
			Usage: "CubeOps server IP list, comma separated",
		},
		cli.StringFlag{
			Name:  "p, port",
			Value: "3010",
			Usage: "CubeOps internal route port",
		},
		cli.DurationFlag{
			Name:  "timeout",
			Value: 35 * time.Second,
			Usage: "HTTP request timeout",
		},
	}
	app.Commands = []cli.Command{
		version.Command,
		node.NodeCommand,
	}
	return app
}

func Run() error {
	return New().Run(os.Args)
}

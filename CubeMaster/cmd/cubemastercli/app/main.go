// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package app provides the main entry point for the CubeMaster CLI.
package app

import (
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/cmd/cubemastercli/commands/cubebox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/cmd/cubemastercli/commands/version"
	pkgv "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version"
	"github.com/urfave/cli"
)

var extraCmds = []cli.Command{}

func init() {
	cli.VersionPrinter = func(c *cli.Context) {
		fmt.Println(c.App.Name, pkgv.Package, c.App.Version)
	}
}

const usage = `cubemastercli --help`

func New() *cli.App {
	app := cli.NewApp()
	app.Name = "cubemastercli"
	app.Version = pkgv.ShowVersion()
	app.Usage = usage
	app.Description = `cubemastercli cli tools`
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "address, a",
			Value: "0.0.0.0",
			Usage: "addresses for cubemaster server, ip1,ip2,ip3,default 0.0.0.0",
		},
		cli.StringFlag{
			Name:  "port, p",
			Value: "8089",
			Usage: "port for cubemaster server",
		},
		cli.DurationFlag{
			Name:  "timeout",
			Value: 35 * time.Second,
			Usage: "total timeout for ctr commands",
		},
	}
	app.Commands = append([]cli.Command{
		version.Command,
		cubebox.Command,
		cubebox.MultiRun,
		cubebox.ListCommand,
		cubebox.InfoCommand,
		cubebox.SandboxCommand,
		cubebox.SnapshotCommand,
		cubebox.StorageCommand,
		cubebox.OperationCommand,
		cubebox.NodeCommand,
		cubebox.TemplateCommand,
		cubebox.ListInventoryCommand,
	}, extraCmds...)
	app.Before = func(context *cli.Context) error {
		return nil
	}
	return app
}

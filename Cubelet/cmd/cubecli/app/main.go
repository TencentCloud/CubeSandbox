// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package app

import (
	"fmt"
	"time"

	namespacesCmd "github.com/containerd/containerd/v2/cmd/ctr/commands/namespaces"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/urfave/cli/v2"

	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands/container"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands/ctrcommands"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands/image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands/metadata"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands/network"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands/storage"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands/unsafe"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands/version"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands/vm"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands/volume"
	pkgv "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/version"
)

var extraCmds = []*cli.Command{

	namespacesCmd.Command,
}

func init() {
	cli.VersionPrinter = func(c *cli.Context) {
		fmt.Println(pkgv.VersionString("cubecli"))
	}
}

const usage = `cubecli --help`

func New() *cli.App {
	app := cli.NewApp()
	app.Name = "cubecli"
	app.Version = pkgv.ShowVersion()
	app.Usage = usage
	app.Description = `cubelet cli tools`
	app.Flags = []cli.Flag{
		&cli.BoolFlag{
			Name:    "debug",
			Aliases: []string{"l"},
			Usage:   "enable debug output in logs",
		},
		&cli.StringFlag{
			Name:    "address",
			Aliases: []string{"a", "unixaddress"},
			Usage:   "address for cubelet's GRPC server (for ctr commands, uses unixaddress if not set)",
			Value:   "/data/cubelet/cubelet.sock",
		},
		&cli.StringFlag{
			Name:    "tcpaddress",
			Aliases: []string{"ta"},
			Usage:   "tcp  address for cubelet's GRPC server (used by ctr commands when address is not set)",
			Value:   "0.0.0.0:9999",
		},
		&cli.StringFlag{
			Name:  "state",
			Usage: "state dir",
			Value: "/data/cubelet/state",
		},
		&cli.DurationFlag{
			Name:  "timeout",
			Value: 60 * time.Second,
			Usage: "total timeout for ctr commands",
		},
		&cli.DurationFlag{
			Name:  "connect-timeout",
			Usage: "timeout for connecting to containerd",
		},
		&cli.StringFlag{
			Name:    "namespace",
			Aliases: []string{"n"},
			Usage:   "namespace to use with commands",
			Value:   namespaces.Default,
		},
	}
	app.Commands = append([]*cli.Command{
		version.Command,
		container.Command,
		container.ExecCommand,
		cubebox.Command,
		cubebox.MultiRun,
		unsafe.Command,
		cubebox.ListCommand,
		cubebox.LogsCommand,
		image.ImageCommand(),
		image.GlobalListImageCommand,
		image.Load,
		volume.Command,
		vm.Command,
		storage.Command,
		network.Command,
		metadata.Command,
		ctrcommands.Command,
	}, extraCmds...)
	return app
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package version provides the version of the client and server
package version

import (
	"fmt"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version"
	"github.com/urfave/cli"
)

var Command = cli.Command{
	Name:  "version",
	Usage: "print the client and server versions",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "versiononly,v",
			Usage: "print server version only",
		},
		&cli.BoolFlag{
			Name:  "withclient,c",
			Usage: "print client version",
		},
	},
	Action: func(context *cli.Context) error {
		if context.Bool("versiononly") {
			fmt.Println(version.Version)
			return nil
		}
		if context.Bool("withclient") {
			fmt.Println(version.VersionString("cubemastercli"))
			fmt.Println("  Go version: " + version.GoVersion)
		}
		return nil
	},
}

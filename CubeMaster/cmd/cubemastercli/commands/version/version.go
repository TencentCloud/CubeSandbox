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
	Usage: "print the client version",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "versiononly,v",
			Usage: "print semantic version only",
		},
		&cli.BoolFlag{
			Name:  "withclient,c",
			Usage: "deprecated: client version is printed by default",
		},
	},
	Action: func(context *cli.Context) error {
		if context.Bool("versiononly") {
			fmt.Println(version.Version)
			return nil
		}
		fmt.Println(version.VersionString("cubemastercli"))
		return nil
	},
}

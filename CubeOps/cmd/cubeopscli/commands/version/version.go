// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package version provides the version subcommand for cubeopscli.
package version

import (
	"fmt"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/version"
	"github.com/urfave/cli"
)

var Command = cli.Command{
	Name:  "version",
	Usage: "print the cubeopscli version",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "versiononly, v",
			Usage: "print the semantic version only",
		},
	},
	Action: func(context *cli.Context) error {
		if context.Bool("versiononly") {
			fmt.Println(version.Version)
			return nil
		}
		fmt.Println(version.VersionString("cubeopscli"))
		return nil
	},
}

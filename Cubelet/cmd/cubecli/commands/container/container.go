// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package container

import (
	"github.com/urfave/cli/v2"
)

var Command = &cli.Command{
	Name:    "container",
	Aliases: []string{"c", "ctr"},
	Usage:   "manage cubebox",
	Subcommands: []*cli.Command{
		ListCommand,
		InfoCommand,
		ExecCommand,
		ListTapCommand,
	},
}

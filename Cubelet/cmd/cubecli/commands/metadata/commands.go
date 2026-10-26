// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package metadata

import "github.com/urfave/cli/v2"

var Command = &cli.Command{
	Name:    "meta",
	Aliases: []string{"m"},
	Usage:   "manage metadata, warning: this command is readonly,\n do not use it to update metadata!!!!!",
	Subcommands: cli.Commands{
		&ShowDbCommand,
		&ListDbCommand,
	},
}

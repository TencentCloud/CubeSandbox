// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package cubebox provides a CLI for managing cubebox.
package cubebox

import (
	"github.com/urfave/cli"
)

var Command = cli.Command{
	Name:    "cubebox",
	Aliases: []string{"cubebox"},
	Usage:   "manage cubebox",
	Subcommands: cli.Commands{
		MultiRun,
		ListCommand,
		InfoCommand,
		DestroyCommand,
		NodeCommand,
		TemplateCommand,
	},
}

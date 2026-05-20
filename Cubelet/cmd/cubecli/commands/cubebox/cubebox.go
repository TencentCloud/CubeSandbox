// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"github.com/urfave/cli/v2"
)

var Command = &cli.Command{
	Name:    "cubebox",
	Aliases: []string{"b"},
	Usage:   "manage cubebox",
	Subcommands: []*cli.Command{
		ListCommand,
		ListSandboxCommand,
		update,
		Create,
		&inspecMetaData,
		Snapshot,
	},
}

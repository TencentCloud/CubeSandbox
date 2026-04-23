// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"github.com/urfave/cli/v2"
)

var Command = &cli.Command{
	Name:    "storage",
	Aliases: []string{"s"},
	Usage:   "Manage storage",
	Subcommands: []*cli.Command{
		lsdb,
		cleanup,
	},
}

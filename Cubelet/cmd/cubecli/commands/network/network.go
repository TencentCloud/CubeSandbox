// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package network

import (
	"github.com/urfave/cli/v2"
)

var Command = &cli.Command{
	Name:    "network",
	Aliases: []string{"n"},
	Usage:   "network operations",
	Subcommands: []*cli.Command{
		list,
	},
}

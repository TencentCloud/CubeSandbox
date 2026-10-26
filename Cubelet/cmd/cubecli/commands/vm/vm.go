// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package vm

import (
	"github.com/urfave/cli/v2"
)

var Command = &cli.Command{
	Name:  "vm",
	Usage: "manage vm",
	Subcommands: []*cli.Command{
		CounterCommand,
	},
}

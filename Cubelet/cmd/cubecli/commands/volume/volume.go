// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package volume

import (
	"github.com/urfave/cli/v2"
)

var Command = &cli.Command{
	Name:    "volume",
	Aliases: []string{"v"},
	Usage:   "manage volumes",
	Subcommands: []*cli.Command{
		resetvolumeref,
		resetVolumeRefExec,
	},
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"github.com/containerd/containerd/v2/cmd/ctr/commands/images"
	"github.com/urfave/cli/v2"
)

func ImageCommand() *cli.Command {
	cc := images.Command
	cc.Name = "ctr-image"
	cc.Aliases = []string{"ci"}
	cc.Description = "Simulate the test API of containerd. All operations are in an unstable state, so please use them with extreme caution."

	var command = &cli.Command{
		Name:    "image",
		Aliases: []string{"i"},
		Usage:   "manage images",
		Subcommands: cli.Commands{
			pullImageCommand,
			ListImageCommand,
			imageStatusCommand,
			removeImageCommand,
			imageFsInfoCommand,
			erofsMountImageCommand,
			cc,
		},
	}
	for _, sc := range cc.Subcommands {
		if sc.Name == "label" {
			command.Subcommands = append(command.Subcommands, sc)
			break
		}
	}
	return command
}

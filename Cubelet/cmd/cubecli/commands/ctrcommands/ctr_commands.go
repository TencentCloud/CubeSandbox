// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package ctrcommands

import (
	ctrcontainers "github.com/containerd/containerd/v2/cmd/ctr/commands/containers"
	ctrcontent "github.com/containerd/containerd/v2/cmd/ctr/commands/content"
	ctrdeprecations "github.com/containerd/containerd/v2/cmd/ctr/commands/deprecations"
	ctrevents "github.com/containerd/containerd/v2/cmd/ctr/commands/events"
	ctrimages "github.com/containerd/containerd/v2/cmd/ctr/commands/images"
	ctrinfo "github.com/containerd/containerd/v2/cmd/ctr/commands/info"
	ctrinstall "github.com/containerd/containerd/v2/cmd/ctr/commands/install"
	ctrleases "github.com/containerd/containerd/v2/cmd/ctr/commands/leases"
	ctrnamespaces "github.com/containerd/containerd/v2/cmd/ctr/commands/namespaces"
	ctroci "github.com/containerd/containerd/v2/cmd/ctr/commands/oci"
	ctrplugins "github.com/containerd/containerd/v2/cmd/ctr/commands/plugins"
	ctrpprof "github.com/containerd/containerd/v2/cmd/ctr/commands/pprof"
	ctrrun "github.com/containerd/containerd/v2/cmd/ctr/commands/run"
	ctrsandboxes "github.com/containerd/containerd/v2/cmd/ctr/commands/sandboxes"
	ctrsnapshots "github.com/containerd/containerd/v2/cmd/ctr/commands/snapshots"
	ctrtasks "github.com/containerd/containerd/v2/cmd/ctr/commands/tasks"
	"github.com/urfave/cli/v2"
)

var Command = &cli.Command{
	Name:    "containerd-ctr",
	Aliases: []string{"containerd", "tr"},
	Usage:   "containerd CLI commands (ctr compatibility)",
	Description: `ctr is an unsupported debug and administrative client for interacting
with the containerd daemon. These commands provide full compatibility with ctr.

All ctr commands are available as subcommands. The 'address' flag used by ctr
commands will automatically use cubecli's 'unixaddress' flag value if 'address'
is not explicitly set.`,
	Subcommands: []*cli.Command{
		ctrplugins.Command,
		ctrcontainers.Command,
		ctrcontent.Command,
		ctrevents.Command,
		ctrimages.Command,
		ctrleases.Command,
		ctrnamespaces.Command,
		ctrpprof.Command,
		ctrrun.Command,
		ctrsnapshots.Command,
		ctrtasks.Command,
		ctrinstall.Command,
		ctroci.Command,
		ctrsandboxes.Command,
		ctrinfo.Command,
		ctrdeprecations.Command,
	},
}

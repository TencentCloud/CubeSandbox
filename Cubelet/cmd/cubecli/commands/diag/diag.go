// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package diag holds host-level diagnostics for sandbox resources that were
// not reclaimed automatically.
//
// These commands read the host directly rather than asking cubelet, because
// the situation they exist for is the one where cubelet has already given up:
// a sandbox is quarantined, its records may be gone, and the only remaining
// evidence is which process still holds the fd.
package diag

import (
	"github.com/urfave/cli/v2"
)

var Command = &cli.Command{
	Name:  "diag",
	Usage: "diagnose and reclaim sandbox resources that cleanup could not release",
	Subcommands: []*cli.Command{
		tapHoldersCommand,
		reclaimCommand,
	},
}

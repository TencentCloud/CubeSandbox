// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package version carries build metadata injected via -ldflags -X at build
// time (see the component Makefile). Mirrors the CubeSandbox convention used
// by network-agent et al.
package version

import "fmt"

var (
	// Version is the release/tag, e.g. "v1.2.3" or "0.0.0-dev".
	Version = "0.0.0-dev"
	// Commit is the git commit hash the binary was built from.
	Commit = "unknown"
	// BuildTime is an RFC3339 UTC timestamp of the build.
	BuildTime = "unknown"
)

// String renders the full version line for `--version`.
func String() string {
	return fmt.Sprintf("cube-snapshot %s (%s) built at %s", Version, Commit, BuildTime)
}

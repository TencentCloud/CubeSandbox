// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package version provides CubeOps build version information.
package version

import "runtime"

var (
	// Version is injected at build time with -ldflags.
	Version = "0.0.0-dev"
	// Commit is injected at build time with -ldflags.
	Commit = "unknown"
	// BuildTime is injected at build time with -ldflags.
	BuildTime = "unknown"
	GoVersion = runtime.Version()
)

// ShowVersion returns the semantic version used by cubelog records.
func ShowVersion() string {
	return Version
}

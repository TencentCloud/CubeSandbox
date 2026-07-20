// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package volume

import (
	"context"
)

// PluginType describes how a VolumePlugin implementation is loaded.
type PluginType string

const (
	// PluginTypeBuiltin means the plugin is compiled directly into cubelet.
	PluginTypeBuiltin PluginType = "builtin"
	// PluginTypeBinary means the plugin is an external process that speaks
	// the newline-delimited JSON wire protocol over stdin/stdout.
	PluginTypeBinary PluginType = "binary"
	// PluginTypeRPC means the plugin is a separate process exposing
	// the VolumePluginService gRPC interface over a Unix socket or TCP.
	PluginTypeRPC PluginType = "rpc"
)

// VolumePlugin is the single interface every plugin must satisfy, regardless
// of how it is loaded (built-in, binary, or RPC).
//
// Lifecycle:
//
//	Init  →  (Attach / Detach)*  →  Close
type VolumePlugin interface {
	// Name returns the canonical driver name that matches
	// PluginVolumeSource.driver in the proto, e.g. "nfs", "s3fuse", "cfs".
	// The Manager routes by exact string equality.
	Name() string

	// PluginType reports the loading mechanism for diagnostics/logging.
	PluginType() PluginType

	// Init is called exactly once when the plugin is registered.
	//
	//   - Built-in plugins use cfg.Extra for any Go-level configuration.
	//   - Binary plugins record cfg.BinaryPath; no subprocess is started until Attach/Detach.
	//   - RPC plugins dial cfg.SocketPath.
	//
	// Init must not block indefinitely; use ctx for deadline enforcement.
	Init(ctx context.Context, cfg PluginConfig) error

	// Attach provisions the volume on the host on behalf of sandboxID and
	// returns metadata that cubelet persists in StorageInfo.
	// Must be idempotent: calling Attach twice with the same
	// (SandboxID, VolumeID) must not create duplicate resources.
	//
	// req.RefCount is the pre-attach count (0 = first attach).
	Attach(ctx context.Context, req *AttachRequest) (*AttachResult, error)

	// Detach tears down the volume attachment for sandboxID.
	// req.Metadata is the exact map returned by the corresponding Attach call
	// so the plugin can locate its resources.
	//
	// req.RefCount is the post-detach count (0 = last detach).
	Detach(ctx context.Context, req *DetachRequest) error

	// Close is called on cubelet shutdown.  Release all resources (subprocess,
	// gRPC connection, open files, etc.).
	Close() error
}

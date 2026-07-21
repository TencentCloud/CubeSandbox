// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package volume

// PluginConfig holds the per-plugin configuration loaded from the containerd
// config TOML under the storage plugin section.
//
// Example TOML:
//
//	[[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
//	  name        = "nfs"
//	  type        = "binary"
//	  binary_path = "/usr/local/bin/cube-volume-nfs"
//
//	[[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
//	  name        = "s3fuse"
//	  type        = "rpc"
//	  socket_path = "/run/cube/volume-s3fuse.sock"
//
//	[[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
//	  name        = "mybuiltin"
//	  type        = "builtin"   # registered in code; no binary_path / socket_path needed
//	  [plugins."io.cubelet.internal.v1.storage".volume_plugins.extra]
//	    timeout = "30s"
type PluginConfig struct {
	// Name is the driver name; must be unique among all volume_plugins entries.
	// The SDK/API routes volumes by driver only — type does not disambiguate.
	Name string `toml:"name"`
	// Type selects the loading mechanism: "builtin", "binary", or "rpc".
	Type PluginType `toml:"type"`
	// BinaryPath is the filesystem path to the plugin executable.
	// Required when Type == PluginTypeBinary.
	BinaryPath string `toml:"binary_path"`
	// SocketPath is the Unix socket (or host:port) for the gRPC server.
	// Required when Type == PluginTypeRPC.
	SocketPath string `toml:"socket_path"`
	// Extra is an opaque key-value map forwarded to the plugin on Init.
	// Plugins may define their own keys; cubelet does not interpret them.
	Extra map[string]string `toml:"extra"`
}

// AttachRequest carries everything a plugin needs to attach a single volume
// to a sandbox.
type AttachRequest struct {
	// SandboxID is the sandbox being created.
	SandboxID string
	// Namespace is the containerd namespace.
	Namespace string
	// VolumeID is the CubeMaster VolumeRecord identifier.
	// Used as the ref-count DB key; unique across all sandboxes and plugins.
	VolumeID string
	// Driver is used by Manager for plugin routing and ref-count bookkeeping.
	// It is not passed to binary/rpc plugin Hook invocations.
	Driver string
	// RefCount is the number of sandboxes already attached to this volume
	// BEFORE this call (pre-attach count).
	// The Manager fills this in; the plugin uses it to detect first-attach.
	//
	// 0 → first attach: volume not yet mounted on the host.
	//     Plugin SHOULD perform host-level setup (e.g. mount NFS share).
	// >0 → already attached by other sandboxes; reuse existing host mount.
	RefCount int64

	// NodeRefFirstAttach is set by Manager.Attach to true when this attach
	// took the node-local ref-count from 0 to 1, i.e. this is the first
	// sandbox on this node to reference the volume.  Cubelet uses it to
	// notify CubeMaster to increment its cross-node ref-count by one.
	NodeRefFirstAttach bool

	// VolumeBaseDir is the parent directory Cubelet requires the plugin to
	// mount this volume under.  The plugin MUST return a HostPath located
	// inside this directory (typically "<VolumeBaseDir>/<plugin-name>-<volumeID>").
	// Cubelet rejects the attach if HostPath is not within VolumeBaseDir.
	VolumeBaseDir string
}

// AttachResult is returned by VolumePlugin.Attach.
type AttachResult struct {
	// VolumeID echoes AttachRequest.VolumeID.
	VolumeID string
	// HostPath is the host-side path that cubelet exposes into the VM,
	// for example via virtiofs shared_dir or a direct bind mount.
	// The plugin is responsible for ensuring this path exists and is
	// populated before returning.
	HostPath string
	// Metadata is opaque key-value data that cubelet persists in
	// PluginVolumeBackendInfo.Metadata and passes back unchanged to
	// Detach.  Use it to record any state needed for cleanup (mount
	// points, remote leases, device names, etc.).
	Metadata map[string]string
}

// DetachRequest is the counterpart of AttachRequest.
type DetachRequest struct {
	SandboxID string
	Namespace string
	// VolumeID is the CubeMaster VolumeRecord identifier — same value that
	// was passed in the corresponding AttachRequest.
	VolumeID string
	Driver   string
	// Metadata is the exact map returned by the corresponding Attach call.
	Metadata map[string]string
	// RefCount is the number of sandboxes still attached AFTER this call
	// (post-detach count).
	// The Manager fills this in after decrementing the count.
	//
	// 0 → last detach: no other sandbox holds the volume.
	//     Plugin SHOULD tear down host-level resources (unmount, release lease).
	// >0 → other sandboxes still attached; plugin SHOULD NOT unmount or delete.
	//
	// Cubelet guarantees Detach is called exactly once per successful Attach,
	// so plugins can use RefCount to drive their own state machines.
	RefCount int64

	// NodeRefLastDetach is set by Manager.Detach to true when this detach took
	// the node-local ref-count from 1 to 0, i.e. this was the last sandbox on
	// this node referencing the volume.  Cubelet uses it to notify CubeMaster
	// to decrement its cross-node ref-count by one.
	NodeRefLastDetach bool
}

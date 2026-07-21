// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package plugin defines the Controller Hook Plugin interface for Volume
// lifecycle management (create / destroy / get-info / exists).
//
// Three implementation styles are supported (mirroring the design doc):
//   - built-in:    compiled directly into CubeMaster (this package)
//   - binary:      CubeMaster forks an external binary with sub-commands
//   - network RPC: CubeMaster calls a remote gRPC/HTTP service
//
// Only built-in plugins register via init(); binary and rpc load from config.
package plugin

import (
	"context"
	"fmt"
)

// ─── Plugin types ──────────────────────────────────────────────────────────

// PluginType selects the loading mechanism for a ControllerPlugin.
type PluginType string

const (
	PluginTypeBuiltin PluginType = "builtin"
	PluginTypeBinary  PluginType = "binary"
	PluginTypeRPC     PluginType = "rpc"
)

// Config holds the configuration for one external (binary) plugin entry,
// as declared in the CubeMaster TOML config file.
//
// Example:
//
//	[[volume_plugins]]
//	  name        = "cos"
//	  type        = "binary"
//	  binary_path = "/usr/local/services/cubetoolbox/volume-plugin/cube-volume-cos"
//
//	[[volume_plugins]]
//	  name        = "cos-rpc"
//	  type        = "rpc"
//	  socket_path = "/run/cube-volume-cos-rpc.sock"
type Config struct {
	// Name is the driver identifier (POST /volumes `driver`, sandbox volumeMounts routing).
	// Must be unique among all volume_plugins entries; type does not disambiguate.
	Name string `toml:"name" yaml:"name"`
	// Type selects the implementation: "builtin", "binary", or "rpc".
	Type PluginType `toml:"type" yaml:"type"`
	// BinaryPath is the filesystem path to the plugin executable.
	// Required when Type == PluginTypeBinary.
	BinaryPath string `toml:"binary_path" yaml:"binary_path"`
	// SocketPath is the Unix socket or TCP address for the gRPC server.
	// Required when Type == PluginTypeRPC.
	SocketPath string `toml:"socket_path" yaml:"socket_path"`
}

// ValidateConfigs checks volume_plugins for duplicate driver names and empty names.
func ValidateConfigs(configs []Config) error {
	seen := make(map[string]PluginType, len(configs))
	for _, cfg := range configs {
		if cfg.Name == "" {
			return fmt.Errorf("volume plugin: name is empty")
		}
		if prev, dup := seen[cfg.Name]; dup {
			return fmt.Errorf(
				"volume plugin %q: duplicate driver name (already declared as type %q); "+
					"each plugin must have a unique name because the SDK selects plugins by driver only",
				cfg.Name, prev,
			)
		}
		seen[cfg.Name] = cfg.Type
	}
	return nil
}

// LoadBinary creates and registers a binary ControllerPlugin from cfg.
// Returns an error if the config is invalid or the name is already registered.
func LoadBinary(cfg Config) error {
	if cfg.Name == "" {
		return fmt.Errorf("volume plugin config: name is empty")
	}
	if cfg.BinaryPath == "" {
		return fmt.Errorf("volume plugin %q: binary_path is empty", cfg.Name)
	}
	if _, dup := registry[cfg.Name]; dup {
		return fmt.Errorf("volume plugin %q already registered", cfg.Name)
	}
	// Import cycle avoided by using a factory func registered by the binary package.
	if binaryFactory == nil {
		return fmt.Errorf("binary volume plugin support not linked in (import _ \"…/plugin/binary\")")
	}
	Register(binaryFactory(cfg.Name, cfg.BinaryPath))
	return nil
}

// LoadRPC creates and registers an RPC ControllerPlugin from cfg.
func LoadRPC(cfg Config) error {
	if cfg.Name == "" {
		return fmt.Errorf("volume plugin config: name is empty")
	}
	if cfg.SocketPath == "" {
		return fmt.Errorf("volume plugin %q: socket_path is empty", cfg.Name)
	}
	if _, dup := registry[cfg.Name]; dup {
		return fmt.Errorf("volume plugin %q already registered", cfg.Name)
	}
	if rpcFactory == nil {
		return fmt.Errorf("rpc volume plugin support not linked in (import _ \"…/plugin/rpc\")")
	}
	p, err := rpcFactory(cfg.Name, cfg.SocketPath)
	if err != nil {
		return err
	}
	Register(p)
	return nil
}

// binaryFactory is set by the binary sub-package via SetBinaryFactory to avoid
// an import cycle between plugin and plugin/binary.
var binaryFactory func(name, binaryPath string) ControllerPlugin

// SetBinaryFactory is called from plugin/binary's init() to register the
// constructor without creating an import cycle.
func SetBinaryFactory(f func(name, binaryPath string) ControllerPlugin) {
	binaryFactory = f
}

// rpcFactory is set by the rpc sub-package via SetRPCFactory.
var rpcFactory func(name, socketPath string) (ControllerPlugin, error)

// SetRPCFactory is called from plugin/rpc's init() to register the
// constructor without creating an import cycle.
func SetRPCFactory(f func(name, socketPath string) (ControllerPlugin, error)) {
	rpcFactory = f
}

// ─── Wire types ────────────────────────────────────────────────────────────

// VolumeInfo is the canonical result returned by the plugin after a
// successful create or get-info call.  All fields except Token are required.
type VolumeInfo struct {
	// VolumeID is the stable identifier assigned by the plugin (or generated
	// by the caller before invoking the plugin).
	VolumeID string `json:"volumeID"`

	// Name is the human-readable label.
	Name string `json:"name"`

	// Token is the auth credential used by the volume-content service.
	// May be empty for backends that do not require per-volume tokens.
	Token string `json:"token"`

	// PluginName identifies which plugin produced this record.
	// Populated automatically by the registry before returning to the caller.
	PluginName string `json:"pluginName"`
}

// ─── Interface ─────────────────────────────────────────────────────────────

// ControllerPlugin is the interface every Controller Hook Plugin must satisfy.
// It handles the management plane: create / destroy.
// The data plane (attach/detach) is handled by the Node Hook in Cubelet.
type ControllerPlugin interface {
	// Name returns the unique plugin identifier stored in VolumeRecord.PluginName.
	Name() string

	// Create allocates a new volume.
	// volumeID is pre-generated by the caller (UUIDv4) so the plugin can use
	// it as a stable handle during creation without a second round-trip.
	Create(ctx context.Context, volumeID, name string) (*VolumeInfo, error)

	// Destroy permanently deletes a volume and all its data.
	Destroy(ctx context.Context, volumeID string) error
}

// BuiltinDemoPluginName is the registered name of the built-in demo plugin.
const BuiltinDemoPluginName = "builtin"

// ─── Registry ──────────────────────────────────────────────────────────────

var registry = map[string]ControllerPlugin{}
var registryOrder []string // preserves insertion order for First()

// Register adds a plugin to the global registry.
// Typically called from an init() function in the plugin's own file.
// Panics on duplicate names to catch misconfiguration at startup.
func Register(p ControllerPlugin) {
	if _, dup := registry[p.Name()]; dup {
		panic("volume controller plugin already registered: " + p.Name())
	}
	registry[p.Name()] = p
	registryOrder = append(registryOrder, p.Name())
}

// Get returns the named plugin, or (nil, false) if not registered.
func Get(name string) (ControllerPlugin, bool) {
	p, ok := registry[name]
	return p, ok
}

// First returns the first registered plugin (in registration order).
// Returns (nil, false) when no plugin has been registered yet.
func First() (ControllerPlugin, bool) {
	if len(registryOrder) == 0 {
		return nil, false
	}
	return registry[registryOrder[0]], true
}

// Default returns the built-in demo plugin used when no explicit plugin is
// configured.  Panics if the built-in plugin was never registered.
func Default() ControllerPlugin {
	p, ok := registry[BuiltinDemoPluginName]
	if !ok {
		panic("built-in volume controller plugin is not registered; import _ \"…/plugin/builtin\"")
	}
	return p
}

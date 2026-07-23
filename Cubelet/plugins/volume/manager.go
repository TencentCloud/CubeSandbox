// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package volume

import (
	"context"
	"fmt"
	"sync"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume/refcount"
)

// Manager is the process-wide registry that routes Attach / Detach calls to
// the correct VolumePlugin by driver name.
//
// Built-in plugins are added via Register() at program init time (or inside
// the storage plugin's InitFn).  Binary and RPC plugins are added after their
// PluginConfig is parsed from TOML.
//
// Routing rule: exact match on VolumePlugin.Name() == PluginVolumeSource.driver.
// If two plugins share the same name the first-registered one wins.
//
// # Reference counting
//
// A plugin volume can be concurrently shared by multiple sandboxes.  Manager
// maintains a persistent RefCountStore; every Attach call does an Acquire and
// every Detach call does a Release before forwarding to the plugin.  The
// resulting RefCount is embedded in AttachRequest / DetachRequest so that
// plugins can decide:
//
//   - Attach: RefCount == 1 → first attach, perform host-level setup.
//   - Detach: RefCount == 0 → last detach, safe to tear down host resources.
//
// # Concurrency
//
// Attach and Detach on the same volumeID are serialised by a per-volume lock
// (volMu). This prevents two races:
//
//  1. Concurrent first-attaches: two goroutines both see Before==0 and both
//     attempt host-level setup (e.g. mounting cosfs).
//
//  2. Interleaved attach/detach: a detach that sees After==0 starts tearing
//     down the host mount while a concurrent attach has already incremented
//     the count but has not yet called the plugin.
type Manager struct {
	mu      sync.RWMutex
	byName  map[string]VolumePlugin
	ordered []VolumePlugin // insertion order, for diagnostics
	rcStore *refcount.Store

	// volMu serialises Attach/Detach calls that share the same volumeID.
	volMu sync.Map // map[string]*sync.Mutex
}

var globalManager = &Manager{byName: make(map[string]VolumePlugin)}

// SetRefCountStore attaches a persistent RefCountStore to the global Manager.
// Must be called once during cubelet startup, before any Attach/Detach.
func SetRefCountStore(s *refcount.Store) {
	globalManager.SetRefCountStore(s)
}

// SetRefCountStore attaches a persistent RefCountStore to this Manager.
func (m *Manager) SetRefCountStore(s *refcount.Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rcStore = s
}

// RefCountStore returns the attached RefCountStore, or nil if none has been set.
func (m *Manager) RefCountStore() *refcount.Store {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rcStore
}

// Register adds p to the global Manager.
// Panics if a plugin with the same name is already registered (detected at startup).
func Register(p VolumePlugin) {
	globalManager.Register(p)
}

// Global returns the process-wide Manager.
func Global() *Manager { return globalManager }

// Register adds a plugin to this Manager.
// Panics if a plugin with the same name is already registered.
func (m *Manager) Register(p VolumePlugin) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.byName[p.Name()]; exists {
		panic(fmt.Sprintf("volume plugin %q already registered", p.Name()))
	}
	m.byName[p.Name()] = p
	m.ordered = append(m.ordered, p)
}

// InitAll calls Init on every registered plugin with its corresponding config.
// configs is keyed by plugin name; missing entries produce a zero PluginConfig.
// Called once from the storage plugin InitFn after all plugins are registered.
func (m *Manager) InitAll(ctx context.Context, configs map[string]PluginConfig) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, p := range m.ordered {
		cfg := configs[p.Name()] // zero value if absent — valid for built-ins
		if err := p.Init(ctx, cfg); err != nil {
			return fmt.Errorf("volume plugin %q init: %w", p.Name(), err)
		}
	}
	return nil
}

// Attach looks up the plugin for req.Driver, acquires a ref-count for the
// (namespace, volumeName) pair, fills in req.RefCount, then calls the plugin.
//
// The ref-count is incremented BEFORE calling the plugin so that concurrent
// Destroy calls on other sandboxes see the correct count.
//
// If the plugin returns an error the ref-count is rolled back (Release).
func (m *Manager) Attach(ctx context.Context, req *AttachRequest) (*AttachResult, error) {
	p, err := m.lookup(req.Driver)
	if err != nil {
		return nil, err
	}

	// Serialise Attach/Detach for the same volumeID to prevent concurrent
	// first-attach races and interleaved attach/detach on the host mount.
	volID := req.VolumeID
	unlock := m.lockVolume(volID)
	defer unlock()

	// Acquire ref-count and pass the BEFORE value to the plugin.
	// before == 0 means this is the first sandbox to attach the volume.
	if rc := m.rcStore; rc != nil {
		ar, err := rc.Acquire(req.Namespace, volID, req.SandboxID, req.Driver)
		if err != nil {
			return nil, fmt.Errorf("plugin_volume %q: acquire refcount: %w", req.VolumeID, err)
		}
		req.RefCount = ar.Before // 0 = first attach
		// 0 → 1 on this node: first sandbox here to reference the volume.
		req.NodeRefFirstAttach = ar.Before == 0 && !ar.AlreadyHeld
	}

	res, pluginErr := p.Attach(ctx, req)
	if pluginErr != nil {
		// Roll back the ref-count on plugin failure. The transition is undone,
		// so the caller must not report a 0→1 event to CubeMaster.
		req.NodeRefFirstAttach = false
		if rc := m.rcStore; rc != nil {
			if _, rbErr := rc.Release(req.Namespace, volID, req.SandboxID); rbErr != nil {
				_ = rbErr
			}
		}
		return nil, pluginErr
	}
	return res, nil
}

// Detach decrements the ref-count for (namespace, volumeName), fills in
// req.RefCount with the count AFTER decrement, then calls the plugin.
//
// If no matching plugin is found the call is a no-op (safe for rollback).
//
// If the plugin Detach fails, the local ref-count is rolled back (re-Acquire)
// and NodeRefLastDetach is cleared so callers must NOT report a 1→0 event to
// CubeMaster. A later successful Detach retry can then emit the transition.
func (m *Manager) Detach(ctx context.Context, req *DetachRequest) error {
	p, err := m.lookup(req.Driver)
	if err != nil {
		// Unknown driver during Detach: release ref-count and continue.
		if rc := m.rcStore; rc != nil {
			rr, _ := rc.Release(req.Namespace, req.VolumeID, req.SandboxID)
			req.NodeRefLastDetach = rr.After == 0 && !rr.NotHeld
		}
		return nil
	}

	// Serialise Attach/Detach for the same volumeID.
	volID := req.VolumeID
	unlock := m.lockVolume(volID)
	defer unlock()

	// Release ref-count and pass the AFTER value to the plugin.
	// after == 0 means this was the last sandbox; host cleanup is appropriate.
	released := false
	if rc := m.rcStore; rc != nil {
		rr, err := rc.Release(req.Namespace, volID, req.SandboxID)
		if err != nil {
			_ = err // non-fatal; don't block Destroy
		} else if !rr.NotHeld {
			released = true
		}
		req.RefCount = rr.After // 0 = last detach
		// 1 → 0 on this node: last sandbox here stopped referencing the volume.
		req.NodeRefLastDetach = rr.After == 0 && !rr.NotHeld
	}

	if pluginErr := p.Detach(ctx, req); pluginErr != nil {
		// Plugin failed: restore the local reference and suppress 1→0 reporting.
		if released {
			if rc := m.rcStore; rc != nil {
				if _, rbErr := rc.Acquire(req.Namespace, volID, req.SandboxID, req.Driver); rbErr != nil {
					_ = rbErr
				}
			}
		}
		req.NodeRefLastDetach = false
		return pluginErr
	}
	return nil
}

// Has reports whether a plugin with the given driver name is registered.
func (m *Manager) Has(driver string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.byName[driver]
	return ok
}

// Close shuts down all registered plugins.  Called on cubelet shutdown.
func (m *Manager) Close() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, p := range m.ordered {
		_ = p.Close()
	}
	return nil
}

// ListDrivers returns the names of all registered plugins.
func (m *Manager) ListDrivers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.byName))
	for name := range m.byName {
		names = append(names, name)
	}
	return names
}

// lockVolume acquires the per-volume mutex for volumeID and returns an unlock
// function.  The lock is lazily created on first use and never deleted (the
// number of distinct volumeIDs is small).
func (m *Manager) lockVolume(volumeID string) func() {
	v, _ := m.volMu.LoadOrStore(volumeID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (m *Manager) lookup(driver string) (VolumePlugin, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p, ok := m.byName[driver]; ok {
		return p, nil
	}
	return nil, &ErrNoPlugin{Driver: driver}
}

// ErrNoPlugin is returned when no registered plugin matches the driver name.
type ErrNoPlugin struct {
	Driver string
}

func (e *ErrNoPlugin) Error() string {
	return fmt.Sprintf("no volume plugin registered for driver %q", e.Driver)
}

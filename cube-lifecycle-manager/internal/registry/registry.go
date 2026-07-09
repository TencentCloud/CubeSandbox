// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package registry holds the in-memory map of every sandbox the sidecar is
// tracking. It is the single source of truth that the sweeper reads to make
// pause decisions and that the resume HTTP handler consults to know whether
// auto-resume is enabled for a given sandbox.
package registry

import (
	"sort"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/lifecycle"
)

// Entry is one row in the registry. LastActiveMs is updated by the
// last-active poller; everything else comes from CubeMaster events.
type Entry struct {
	Meta lifecycle.SandboxLifecycleMeta

	// LastActiveMs is the most recent activity timestamp seen across all
	// CubeProxy instances (sidecar takes max() over instances). Zero means
	// "never observed" — the sweeper falls back to Meta.CreatedAt for the
	// idle calculation.
	LastActiveMs int64

	// FirstSeenAt is when the sidecar registered the sandbox locally. The
	// sweeper compares this against config.GracePeriod so a freshly-restarted
	// sidecar doesn't pause everything in its first sweep before it has a
	// chance to receive any activity reports.
	FirstSeenAt time.Time
}

// Registry is a goroutine-safe map. Reads happen on every sweep + resume
// request; writes happen on every stream event + last_active poll. We pick
// sync.RWMutex over sync.Map because the sweep wants a deterministic snapshot.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]*Entry
}

func New() *Registry {
	return &Registry{entries: make(map[string]*Entry)}
}

// Upsert installs (or replaces) the lifecycle meta for a sandbox. Existing
// LastActiveMs is preserved so a stream-replay event doesn't roll back our
// activity view. FirstSeenAt is set on first insert and not overwritten.
func (r *Registry) Upsert(meta lifecycle.SandboxLifecycleMeta) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cur, ok := r.entries[meta.SandboxID]
	if !ok {
		r.entries[meta.SandboxID] = &Entry{
			Meta:        meta,
			FirstSeenAt: time.Now(),
		}
		return
	}
	cur.Meta = meta
}

// Delete drops a sandbox; no-op if absent.
func (r *Registry) Delete(sandboxID string) {
	r.mu.Lock()
	delete(r.entries, sandboxID)
	r.mu.Unlock()
}

// MergeLastActive bumps LastActiveMs to max(current, ts). Returns true when
// the timestamp moved forward, so callers can avoid spurious work.
// Sandboxes that aren't in the registry are ignored — last_active is only
// meaningful for sandboxes CubeMaster has told us about.
func (r *Registry) MergeLastActive(sandboxID string, tsMs int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[sandboxID]
	if !ok {
		return false
	}
	if tsMs > e.LastActiveMs {
		e.LastActiveMs = tsMs
		return true
	}
	return false
}

// ResetLastActive clears LastActiveMs back to 0 for a single sandbox.
func (r *Registry) ResetLastActive(sandboxID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[sandboxID]
	if !ok {
		return false
	}
	e.LastActiveMs = 0
	return true
}

// Get returns a copy of the entry for inspection. Returns nil when absent.
func (r *Registry) Get(sandboxID string) *Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[sandboxID]
	if !ok {
		return nil
	}
	cp := *e
	return &cp
}

// Snapshot returns every entry in a deterministic (sandbox-ID-sorted) order.
// Used by the sweeper. Each returned Entry is a copy — safe to mutate.
func (r *Registry) Snapshot() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]string, 0, len(r.entries))
	for k := range r.entries {
		ids = append(ids, k)
	}
	sort.Strings(ids)

	out := make([]Entry, 0, len(ids))
	for _, id := range ids {
		out = append(out, *r.entries[id])
	}
	return out
}

// Len returns the entry count. Cheap; used by the /metrics endpoint.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// Reset removes every entry. Bootstrap uses it before re-applying a fresh
// HGETALL to avoid leaking stale entries across sidecar restarts within the
// same in-memory state (e.g. tests).
func (r *Registry) Reset() {
	r.mu.Lock()
	r.entries = make(map[string]*Entry)
	r.mu.Unlock()
}

// SetFirstSeenAt overrides the FirstSeenAt for a sandbox. Production code
// must not call this — FirstSeenAt is meant to be set exactly once on the
// initial Upsert. It exists so tests can backdate the registry past the
// sweeper's grace period without sleeping.
func (r *Registry) SetFirstSeenAt(sandboxID string, t time.Time) {
	r.mu.Lock()
	if e, ok := r.entries[sandboxID]; ok {
		e.FirstSeenAt = t
	}
	r.mu.Unlock()
}

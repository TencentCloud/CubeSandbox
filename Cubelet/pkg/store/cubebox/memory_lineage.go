// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"strings"
)

// MemoryBase is one memory-lineage binding. SnapshotID is never empty;
// a nil *MemoryBase means "no base".
type MemoryBase struct {
	SnapshotID string `json:"snapshot_id"`
}

// MemoryLineage is the sandbox's memory-snapshot ancestry. It is the only
// source of truth: resolvers never fall back to create-request annotations.
//
// A nil *MemoryLineage is treated as "no memory base" — the same as an empty
// struct. Restart rebuilds a CubeBox with an empty lineage, so the next
// commit or pause is a full memory capture without a special-case invalidate
// call.
type MemoryLineage struct {
	Runtime  *MemoryBase `json:"runtime,omitempty"`
	Restore  *MemoryBase `json:"restore,omitempty"`
	Ancestor *MemoryBase `json:"ancestor,omitempty"`
}

func (l *MemoryLineage) DeepCopy() *MemoryLineage {
	if l == nil {
		return nil
	}
	copied := *l
	copied.Runtime = l.Runtime.DeepCopy()
	copied.Restore = l.Restore.DeepCopy()
	copied.Ancestor = l.Ancestor.DeepCopy()
	return &copied
}

func (b *MemoryBase) DeepCopy() *MemoryBase {
	if b == nil {
		return nil
	}
	copied := *b
	return &copied
}

func (cb *CubeBox) MemoryLineageCopy() *MemoryLineage {
	if cb == nil {
		return nil
	}
	cb.memoryLineageLock.RLock()
	defer cb.memoryLineageLock.RUnlock()
	return cb.MemoryLineage.DeepCopy()
}

func (cb *CubeBox) RestoreMemoryLineage(lineage *MemoryLineage) {
	if cb == nil {
		return
	}
	cb.memoryLineageLock.Lock()
	defer cb.memoryLineageLock.Unlock()
	cb.MemoryLineage = lineage.DeepCopy()
}

// RuntimeBaseID is the snapshot whose memory the next soft-dirty commit
// clones. Empty means there is no incremental base.
func (cb *CubeBox) RuntimeBaseID() string {
	return memoryBaseID(cb, func(l *MemoryLineage) *MemoryBase { return l.Runtime })
}

// RestoreBaseID is the snapshot the running VM was last restored from.
// Commit does not advance it. Empty means pagemap_anon has no base.
func (cb *CubeBox) RestoreBaseID() string {
	return memoryBaseID(cb, func(l *MemoryLineage) *MemoryBase { return l.Restore })
}

// LaunchAncestorID is the template or customer snapshot this sandbox was
// first started from. Pause may clone it only when it is still the last
// restore.
func (cb *CubeBox) LaunchAncestorID() string {
	return memoryBaseID(cb, func(l *MemoryLineage) *MemoryBase { return l.Ancestor })
}

func memoryBaseID(cb *CubeBox, pick func(*MemoryLineage) *MemoryBase) string {
	if cb == nil || pick == nil {
		return ""
	}
	cb.memoryLineageLock.RLock()
	defer cb.memoryLineageLock.RUnlock()
	if cb.MemoryLineage == nil {
		return ""
	}
	base := pick(cb.MemoryLineage)
	if base == nil {
		return ""
	}
	return strings.TrimSpace(base.SnapshotID)
}

// BindRuntimeBase records the snapshot the next incremental commit clones.
// An empty id is a no-op; clearing is ClearRuntimeBase.
func (cb *CubeBox) BindRuntimeBase(snapshotID string) {
	cb.bindMemoryBase(snapshotID, func(l *MemoryLineage, base *MemoryBase) {
		l.Runtime = base
	})
}

// BindRestoreBase records the snapshot the VM was last restored from.
// An empty id is a no-op.
func (cb *CubeBox) BindRestoreBase(snapshotID string) {
	cb.bindMemoryBase(snapshotID, func(l *MemoryLineage, base *MemoryBase) {
		l.Restore = base
	})
}

// BindLaunchAncestorOnce records the start image the first time it is known.
// Later calls do not overwrite it, so Resume cannot replace a customer
// snapshot with the pause package.
func (cb *CubeBox) BindLaunchAncestorOnce(snapshotID string) {
	snapshotID = strings.TrimSpace(snapshotID)
	if cb == nil || snapshotID == "" {
		return
	}
	cb.memoryLineageLock.Lock()
	defer cb.memoryLineageLock.Unlock()
	l := cb.ensureMemoryLineageLocked()
	if l.Ancestor != nil && strings.TrimSpace(l.Ancestor.SnapshotID) != "" {
		return
	}
	l.Ancestor = &MemoryBase{SnapshotID: snapshotID}
}

// ClearRuntimeBase drops only the commit incremental base. The restore base
// stays, so a failed publish after a cleared soft-dirty bitmap forces the
// next commit to a full capture without forgetting the last restore.
func (cb *CubeBox) ClearRuntimeBase() {
	if cb == nil {
		return
	}
	cb.memoryLineageLock.Lock()
	defer cb.memoryLineageLock.Unlock()
	l := cb.ensureMemoryLineageLocked()
	l.Runtime = nil
}

// ClearRuntimeLineage drops the commit base and the last-restore base.
// The launch ancestor is kept so Pause can still name the start image, but
// with no restore base it must not incremental-overlay onto that image.
// Used when the VM memory was replaced from a source Cubelet cannot reflink
// (opaque resume). Restart does not call this: the rebuilt CubeBox simply
// has an empty lineage.
func (cb *CubeBox) ClearRuntimeLineage() {
	if cb == nil {
		return
	}
	cb.memoryLineageLock.Lock()
	defer cb.memoryLineageLock.Unlock()
	l := cb.ensureMemoryLineageLocked()
	l.Runtime = nil
	l.Restore = nil
}

func (cb *CubeBox) bindMemoryBase(snapshotID string, assign func(*MemoryLineage, *MemoryBase)) {
	snapshotID = strings.TrimSpace(snapshotID)
	if cb == nil || assign == nil || snapshotID == "" {
		return
	}
	cb.memoryLineageLock.Lock()
	defer cb.memoryLineageLock.Unlock()
	l := cb.ensureMemoryLineageLocked()
	assign(l, &MemoryBase{SnapshotID: snapshotID})
}

func (cb *CubeBox) ensureMemoryLineageLocked() *MemoryLineage {
	if cb.MemoryLineage == nil {
		cb.MemoryLineage = &MemoryLineage{}
	}
	return cb.MemoryLineage
}

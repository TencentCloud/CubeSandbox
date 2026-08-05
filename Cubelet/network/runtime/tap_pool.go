// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"fmt"
	"net"
	"slices"
	"sync"
)

// TapPoolState is the controller-visible lifecycle of a host TAP.
type TapPoolState string

const (
	// Ready means the TAP has default-deny policy and no sandbox owner; Acquire
	// may reserve it.
	TapPoolReady TapPoolState = "Ready"
	// Active means the TAP belongs to a successful sandbox network.
	TapPoolActive TapPoolState = "Active"
	// Cleaning means the TAP is owned by cleanup and must not be assigned.
	TapPoolCleaning TapPoolState = "Cleaning"
)

// TapPoolEntry is the compact, mutex-protected identity and lifecycle record for
// one TAP. Runtime-only fields such as *os.File live on an Active or in-flight
// managedState, or in the controller's idle-fd registry (tapFds) for pooled
// TAPs; the entry itself always retains identity only.
type TapPoolEntry struct {
	TapName        string
	TapIfIndex     int
	SandboxIP      net.IP
	OwnerSandboxID string
	State          TapPoolState
	RetryCount     int
	LastError      string
}

// NewReadyTapPoolEntry validates the immutable identity fields for a reusable TAP.
func NewReadyTapPoolEntry(tapName string, ifindex int, sandboxIP net.IP) (*TapPoolEntry, error) {
	if tapName == "" {
		return nil, fmt.Errorf("tap name is empty")
	}
	if ifindex <= 0 {
		return nil, fmt.Errorf("tap ifindex must be positive")
	}
	if sandboxIP.To4() == nil {
		return nil, fmt.Errorf("sandbox ip must be IPv4")
	}
	return &TapPoolEntry{TapName: tapName, TapIfIndex: ifindex, SandboxIP: append(net.IP(nil), sandboxIP.To4()...), State: TapPoolReady}, nil
}

// NewTapPoolEntry constructs an entry in any valid state and enforces the owner
// invariant: Ready has no owner, every non-Ready lifecycle state does.
func NewTapPoolEntry(tapName string, ifindex int, sandboxIP net.IP, owner string, state TapPoolState) (*TapPoolEntry, error) {
	entry, err := NewReadyTapPoolEntry(tapName, ifindex, sandboxIP)
	if err != nil {
		return nil, err
	}
	switch state {
	case TapPoolReady:
		if owner != "" {
			return nil, fmt.Errorf("ready tap %s cannot have owner %s", tapName, owner)
		}
	case TapPoolActive, TapPoolCleaning:
		if owner == "" {
			return nil, fmt.Errorf("%s tap %s requires owner", state, tapName)
		}
		entry.OwnerSandboxID = owner
		entry.State = state
	default:
		return nil, fmt.Errorf("unknown tap pool state %q", state)
	}
	return entry, nil
}

// IsAssignable reports whether Acquire may reserve this entry.
func (e *TapPoolEntry) IsAssignable() bool {
	return e != nil && e.State == TapPoolReady && e.OwnerSandboxID == ""
}

// MarkActive publishes a previously Ready/reserved entry as the sandbox's active TAP.
func (e *TapPoolEntry) MarkActive(owner string) error {
	if e == nil {
		return fmt.Errorf("entry is nil")
	}
	if owner == "" {
		return fmt.Errorf("owner is empty")
	}
	if e.State != TapPoolReady {
		return fmt.Errorf("tap %s cannot become Active from state %s", e.TapName, e.State)
	}
	if e.OwnerSandboxID != "" && e.OwnerSandboxID != owner {
		return fmt.Errorf("tap %s owner mismatch: want %s got %s", e.TapName, owner, e.OwnerSandboxID)
	}
	e.OwnerSandboxID = owner
	e.State = TapPoolActive
	e.RetryCount = 0
	e.LastError = ""
	return nil
}

// BeginCleanup transfers the entry to the cleanup owner. It accepts Active and
// already-Cleaning entries so release retries and maintenance retries stay
// idempotent.
func (e *TapPoolEntry) BeginCleanup(owner string) error {
	if e == nil {
		return fmt.Errorf("entry is nil")
	}
	if owner != "" && e.OwnerSandboxID != "" && e.OwnerSandboxID != owner {
		return fmt.Errorf("tap %s owner mismatch: want %s got %s", e.TapName, owner, e.OwnerSandboxID)
	}
	switch e.State {
	case TapPoolReady:
		if e.OwnerSandboxID == "" {
			return fmt.Errorf("tap %s has no lifecycle owner to clean", e.TapName)
		}
		e.State = TapPoolCleaning
		return nil
	case TapPoolActive, TapPoolCleaning:
		e.State = TapPoolCleaning
		if owner != "" {
			e.OwnerSandboxID = owner
		}
		return nil
	default:
		return fmt.Errorf("tap %s cannot begin cleanup from state %s", e.TapName, e.State)
	}
}

// MarkReady returns a successfully-cleaned TAP to the free pool.
func (e *TapPoolEntry) MarkReady() error {
	if e == nil {
		return fmt.Errorf("entry is nil")
	}
	if e.State != TapPoolCleaning {
		return fmt.Errorf("tap %s cannot become Ready from state %s", e.TapName, e.State)
	}
	e.State = TapPoolReady
	e.OwnerSandboxID = ""
	e.RetryCount = 0
	e.LastError = ""
	return nil
}

// TapPool is a small state-machine store. The entries slice preserves stable
// listing/allocation order, while byName and byOwner enforce uniqueness.
type TapPool struct {
	mu      sync.Mutex
	entries []*TapPoolEntry
	byOwner map[string]*TapPoolEntry
	byName  map[string]*TapPoolEntry
}

// NewTapPool creates a pool and validates any initial entries through Add.
func NewTapPool(entries ...*TapPoolEntry) (*TapPool, error) {
	pool := &TapPool{byOwner: make(map[string]*TapPoolEntry), byName: make(map[string]*TapPoolEntry)}
	for _, entry := range entries {
		if err := pool.Add(entry); err != nil {
			return nil, err
		}
	}
	return pool, nil
}

// Add registers a TAP entry and enforces unique tap name and owner indexes.
func (p *TapPool) Add(entry *TapPoolEntry) error {
	if entry == nil {
		return fmt.Errorf("entry is nil")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.byName[entry.TapName]; ok {
		return fmt.Errorf("tap %s already exists", entry.TapName)
	}
	if entry.OwnerSandboxID != "" {
		if _, ok := p.byOwner[entry.OwnerSandboxID]; ok {
			return fmt.Errorf("owner %s already has an entry", entry.OwnerSandboxID)
		}
	}
	p.entries = append(p.entries, entry)
	p.byName[entry.TapName] = entry
	if entry.OwnerSandboxID != "" {
		p.byOwner[entry.OwnerSandboxID] = entry
	}
	return nil
}

// AddReserved atomically adds a freshly-created Ready TAP and reserves it for
// owner before it can be observed by another Acquire call.
func (p *TapPool) AddReserved(entry *TapPoolEntry, owner string) error {
	if entry == nil {
		return fmt.Errorf("entry is nil")
	}
	if owner == "" {
		return fmt.Errorf("owner is empty")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry.State != TapPoolReady || entry.OwnerSandboxID != "" {
		return fmt.Errorf("tap %s must be unowned Ready before AddReserved", entry.TapName)
	}
	if _, ok := p.byName[entry.TapName]; ok {
		return fmt.Errorf("tap %s already exists", entry.TapName)
	}
	if _, ok := p.byOwner[owner]; ok {
		return fmt.Errorf("owner %s already has an entry", owner)
	}
	entry.OwnerSandboxID = owner
	p.entries = append(p.entries, entry)
	p.byName[entry.TapName] = entry
	p.byOwner[owner] = entry
	return nil
}

// Acquire reserves the first Ready entry for owner. The entry remains in Ready
// until CommitActive, which lets create rollback release the reservation without
// treating the TAP as an active sandbox resource.
func (p *TapPool) Acquire(owner string) (*TapPoolEntry, error) {
	if owner == "" {
		return nil, fmt.Errorf("owner is empty")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing := p.byOwner[owner]; existing != nil {
		if existing.State == TapPoolReady || existing.State == TapPoolActive {
			return existing, nil
		}
		return nil, fmt.Errorf("owner %s already has tap %s in state %s", owner, existing.TapName, existing.State)
	}
	idx := slices.IndexFunc(p.entries, func(entry *TapPoolEntry) bool {
		return entry.IsAssignable()
	})
	if idx < 0 {
		return nil, fmt.Errorf("no ready tap entry")
	}
	entry := p.entries[idx]
	entry.OwnerSandboxID = owner
	entry.RetryCount = 0
	entry.LastError = ""
	p.byOwner[owner] = entry
	return entry, nil
}

// ReleaseReservation cancels a pre-success Acquire reservation.
func (p *TapPool) ReleaseReservation(entry *TapPoolEntry, owner string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry == nil {
		return fmt.Errorf("entry is nil")
	}
	if current := p.byName[entry.TapName]; current != entry {
		return fmt.Errorf("tap %s is not managed by this pool", entry.TapName)
	}
	if entry.State != TapPoolReady {
		return fmt.Errorf("tap %s reservation cannot be released from state %s", entry.TapName, entry.State)
	}
	if entry.OwnerSandboxID != owner {
		return fmt.Errorf("tap %s owner mismatch: want %s got %s", entry.TapName, owner, entry.OwnerSandboxID)
	}
	entry.OwnerSandboxID = ""
	delete(p.byOwner, owner)
	return nil
}

// CommitActive publishes a reserved Ready entry as Active for owner.
func (p *TapPool) CommitActive(entry *TapPoolEntry, owner string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry == nil {
		return fmt.Errorf("entry is nil")
	}
	if current := p.byName[entry.TapName]; current != entry {
		return fmt.Errorf("tap %s is not managed by this pool", entry.TapName)
	}
	if err := entry.MarkActive(owner); err != nil {
		return err
	}
	p.byOwner[owner] = entry
	return nil
}

// BeginCleanup finds the owner's entry and moves it to Cleaning.
func (p *TapPool) BeginCleanup(owner string) (*TapPoolEntry, error) {
	if owner == "" {
		return nil, fmt.Errorf("owner is empty")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.byOwner[owner]
	if entry == nil {
		return nil, fmt.Errorf("owner %s not found", owner)
	}
	if err := entry.BeginCleanup(owner); err != nil {
		return nil, err
	}
	delete(p.byOwner, owner)
	return entry, nil
}

// BeginCleanupByName moves a named TAP to Cleaning. Recovery uses this when it
// has a TAP identity from a state file rather than an owner index.
func (p *TapPool) BeginCleanupByName(tapName, owner string) (*TapPoolEntry, error) {
	if tapName == "" {
		return nil, fmt.Errorf("tap name is empty")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.byName[tapName]
	if entry == nil {
		return nil, fmt.Errorf("tap %s not found", tapName)
	}
	if err := entry.BeginCleanup(owner); err != nil {
		return nil, err
	}
	if entry.OwnerSandboxID != "" {
		delete(p.byOwner, entry.OwnerSandboxID)
	}
	return entry, nil
}

// MarkReady makes a Cleaning entry assignable again and clears its owner index.
func (p *TapPool) MarkReady(entry *TapPoolEntry) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry == nil {
		return fmt.Errorf("entry is nil")
	}
	if current := p.byName[entry.TapName]; current != entry {
		return fmt.Errorf("tap %s is not managed by this pool", entry.TapName)
	}
	oldOwner := entry.OwnerSandboxID
	if err := entry.MarkReady(); err != nil {
		return err
	}
	if oldOwner != "" {
		delete(p.byOwner, oldOwner)
	}
	return nil
}

// StateByName returns a snapshot of one TAP's lifecycle state.
func (p *TapPool) StateByName(tapName string) (TapPoolState, string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.byName[tapName]
	if entry == nil {
		return "", "", false
	}
	return entry.State, entry.OwnerSandboxID, true
}

// RecordFailure updates diagnostic retry metadata while holding the pool lock.
// Callers must not mutate entries returned by GetByName/Entries directly.
func (p *TapPool) RecordFailure(tapName string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.byName[tapName]
	if entry == nil {
		return
	}
	entry.RetryCount++
	if err != nil {
		entry.LastError = err.Error()
	}
}

// GetByName returns the pool entry for internal transition code.
func (p *TapPool) GetByName(tapName string) (*TapPoolEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.byName[tapName]
	return entry, ok
}

// Entries returns immutable-by-convention value snapshots for deterministic
// listing and maintenance scans. Deep copies avoid racing pool transitions after
// p.mu is released.
func (p *TapPool) Entries() []*TapPoolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*TapPoolEntry, 0, len(p.entries))
	for _, entry := range p.entries {
		if entry == nil {
			out = append(out, nil)
			continue
		}
		snapshot := *entry
		snapshot.SandboxIP = append(net.IP(nil), entry.SandboxIP...)
		out = append(out, &snapshot)
	}
	return out
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package volume

import (
	"context"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume/refcount"
)

// fakePlugin records the RefCount and NodeRefLastDetach it observes on Detach,
// so tests can assert the Manager's ref-count bookkeeping is correct.
type fakePlugin struct {
	name          string
	attachErr     error
	detachErr     error
	gotRefCount   int64
	gotLastDetach bool
	detachCalls   int
}

func (p *fakePlugin) Name() string                                 { return p.name }
func (p *fakePlugin) PluginType() PluginType                       { return PluginTypeBuiltin }
func (p *fakePlugin) Init(_ context.Context, _ PluginConfig) error { return nil }

func (p *fakePlugin) Attach(_ context.Context, req *AttachRequest) (*AttachResult, error) {
	if p.attachErr != nil {
		return nil, p.attachErr
	}
	return &AttachResult{VolumeID: req.VolumeID, HostPath: "/tmp/" + req.VolumeID}, nil
}

func (p *fakePlugin) Detach(_ context.Context, req *DetachRequest) error {
	p.detachCalls++
	p.gotRefCount = req.RefCount
	p.gotLastDetach = req.NodeRefLastDetach
	return p.detachErr
}

func (p *fakePlugin) Close() error { return nil }

func newStore(t *testing.T) (*refcount.Store, *bolt.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "refcount.db")
	db, err := bolt.Open(dbPath, 0o600, nil)
	if err != nil {
		t.Fatalf("open bbolt: %v", err)
	}
	s, err := refcount.New(db)
	if err != nil {
		_ = db.Close()
		t.Fatalf("new store: %v", err)
	}
	return s, db
}

// When the refcount store fails on Release, Detach must:
//   - pass RefCount=1 to the plugin (not the zero-value 0 the plugin would
//     read as "last detach"),
//   - clear NodeRefLastDetach so CubeMaster is not told 1→0,
//   - still return nil so the caller can reclaim the per-sandbox bind.
func TestDetachRefcountReleaseError(t *testing.T) {
	store, db := newStore(t)
	// Prime a two-holder record so a healthy Release would land on After=1.
	if _, err := store.Acquire("ns", "vol1", "sbxA", "fake"); err != nil {
		t.Fatalf("acquire A: %v", err)
	}
	if _, err := store.Acquire("ns", "vol1", "sbxB", "fake"); err != nil {
		t.Fatalf("acquire B: %v", err)
	}
	// Close the underlying DB so the next Release returns an error.
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	p := &fakePlugin{name: "fake"}
	m := &Manager{byName: map[string]VolumePlugin{}}
	m.Register(p)
	m.SetRefCountStore(store)

	req := &DetachRequest{
		SandboxID: "sbxA",
		Namespace: "ns",
		VolumeID:  "vol1",
		Driver:    "fake",
	}
	if err := m.Detach(context.Background(), req); err != nil {
		t.Fatalf("Detach returned error: %v", err)
	}
	if p.detachCalls != 1 {
		t.Fatalf("plugin Detach called %d times, want 1", p.detachCalls)
	}
	if p.gotRefCount != 1 {
		t.Errorf("plugin got RefCount=%d, want 1 (safe-mode fallback)", p.gotRefCount)
	}
	if p.gotLastDetach {
		t.Error("plugin got NodeRefLastDetach=true, want false (suppressed on refcount error)")
	}
	if req.RefCount != 1 {
		t.Errorf("req.RefCount=%d, want 1", req.RefCount)
	}
	if req.NodeRefLastDetach {
		t.Error("req.NodeRefLastDetach=true, want false")
	}
}

// Sanity: on the healthy path Detach must pass the AFTER count and set
// NodeRefLastDetach when the count reaches zero, so the fix does not regress
// the normal semantics.
func TestDetachRefcountReleaseSuccessLastHolder(t *testing.T) {
	store, db := newStore(t)
	defer db.Close()
	if _, err := store.Acquire("ns", "vol1", "sbxA", "fake"); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	p := &fakePlugin{name: "fake"}
	m := &Manager{byName: map[string]VolumePlugin{}}
	m.Register(p)
	m.SetRefCountStore(store)

	req := &DetachRequest{
		SandboxID: "sbxA",
		Namespace: "ns",
		VolumeID:  "vol1",
		Driver:    "fake",
	}
	if err := m.Detach(context.Background(), req); err != nil {
		t.Fatalf("Detach returned error: %v", err)
	}
	if p.gotRefCount != 0 {
		t.Errorf("plugin got RefCount=%d, want 0 (last detach)", p.gotRefCount)
	}
	if !p.gotLastDetach {
		t.Error("plugin got NodeRefLastDetach=false, want true (last holder)")
	}
}

// Sanity: on the healthy path with another holder still present, the plugin
// must see the AFTER count as 1 and NodeRefLastDetach must be false.
func TestDetachRefcountReleaseSuccessMultiHolder(t *testing.T) {
	store, db := newStore(t)
	defer db.Close()
	if _, err := store.Acquire("ns", "vol1", "sbxA", "fake"); err != nil {
		t.Fatalf("acquire A: %v", err)
	}
	if _, err := store.Acquire("ns", "vol1", "sbxB", "fake"); err != nil {
		t.Fatalf("acquire B: %v", err)
	}

	p := &fakePlugin{name: "fake"}
	m := &Manager{byName: map[string]VolumePlugin{}}
	m.Register(p)
	m.SetRefCountStore(store)

	req := &DetachRequest{
		SandboxID: "sbxA",
		Namespace: "ns",
		VolumeID:  "vol1",
		Driver:    "fake",
	}
	if err := m.Detach(context.Background(), req); err != nil {
		t.Fatalf("Detach returned error: %v", err)
	}
	if p.gotRefCount != 1 {
		t.Errorf("plugin got RefCount=%d, want 1 (one holder still present)", p.gotRefCount)
	}
	if p.gotLastDetach {
		t.Error("plugin got NodeRefLastDetach=true, want false")
	}
}

// Unknown-driver Detach must also suppress NodeRefLastDetach when the refcount
// store fails, so CubeMaster is never told 1→0 on an unrecovered error.
func TestDetachUnknownDriverRefcountReleaseError(t *testing.T) {
	store, db := newStore(t)
	if _, err := store.Acquire("ns", "vol1", "sbxA", "fake"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	m := &Manager{byName: map[string]VolumePlugin{}}
	m.SetRefCountStore(store)

	req := &DetachRequest{
		SandboxID: "sbxA",
		Namespace: "ns",
		VolumeID:  "vol1",
		Driver:    "missing",
	}
	if err := m.Detach(context.Background(), req); err != nil {
		t.Fatalf("Detach returned error: %v", err)
	}
	if req.NodeRefLastDetach {
		t.Error("req.NodeRefLastDetach=true, want false (suppressed on refcount error)")
	}
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStateStoreUsesPrivatePermissions(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shardPath := filepath.Join(stateDir, shardOf("old"), "old.success.json")
	if err := os.MkdirAll(filepath.Dir(shardPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shardPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := newStateStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	assertFileMode(t, stateDir, 0o700)
	assertFileMode(t, shardPath, 0o600)

	state := testPersistedState("sandbox1")
	if err := store.WriteTmp(state); err != nil {
		t.Fatal(err)
	}
	tmpPath, err := store.path(state.SandboxID, StateFileTmp)
	if err != nil {
		t.Fatal(err)
	}
	assertFileMode(t, tmpPath, 0o600)
}

func TestStateStoreLifecycleRenames(t *testing.T) {
	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := testPersistedState("sandbox1")

	if err := store.WriteTmp(state); err != nil {
		t.Fatal(err)
	}
	assertStateFile(t, store, state.SandboxID, StateFileTmp, true)
	assertStateFile(t, store, state.SandboxID, StateFileCreating, false)

	if err := store.CommitCreating(state.SandboxID); err != nil {
		t.Fatal(err)
	}
	assertStateFile(t, store, state.SandboxID, StateFileTmp, false)
	assertStateFile(t, store, state.SandboxID, StateFileCreating, true)

	if err := store.CommitSuccess(state.SandboxID); err != nil {
		t.Fatal(err)
	}
	assertStateFile(t, store, state.SandboxID, StateFileCreating, false)
	assertStateFile(t, store, state.SandboxID, StateFileSuccess, true)

	if err := store.MarkDeleting(state.SandboxID); err != nil {
		t.Fatal(err)
	}
	assertStateFile(t, store, state.SandboxID, StateFileSuccess, false)
	assertStateFile(t, store, state.SandboxID, StateFileDeleting, true)
}

func TestStateStoreScanIncludesLifecycleKinds(t *testing.T) {
	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeStateAs(t, store, testPersistedState("tmp-sandbox"), StateFileTmp)
	writeStateAs(t, store, testPersistedState("creating-sandbox"), StateFileCreating)
	writeStateAs(t, store, testPersistedState("success-sandbox"), StateFileSuccess)
	writeStateAs(t, store, testPersistedState("deleting-sandbox"), StateFileDeleting)

	records, err := store.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 {
		t.Fatalf("records length = %d", len(records))
	}
	got := map[StateFileKind]bool{}
	for _, record := range records {
		got[record.Kind] = true
	}
	for _, kind := range allStateFileKinds {
		if !got[kind] {
			t.Fatalf("missing kind %s in scan result", kind)
		}
	}
}

func TestStateStoreRejectsUnsafeSandboxID(t *testing.T) {
	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	unsafeIDs := []string{"", "../x", "a/b", `a\\b`, "a.b"}
	for _, id := range unsafeIDs {
		state := testPersistedState(id)
		if err := store.WriteTmp(state); err == nil {
			t.Fatalf("WriteTmp(%q) succeeded, want error", id)
		}
	}
}

func TestStateStoreScanSkipsCorruptFilesWithoutDeletingPotentialInflightTmp(t *testing.T) {
	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	valid := testPersistedState("valid-sandbox")
	writeStateAs(t, store, valid, StateFileSuccess)
	corruptSuccessPath, err := store.path("bad-success", StateFileSuccess)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(corruptSuccessPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptSuccessPath, []byte(`{"sandboxID":"bad-success",`), 0o644); err != nil {
		t.Fatal(err)
	}
	corruptTmpPath, err := store.path("bad-tmp", StateFileTmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(corruptTmpPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptTmpPath, []byte(`{"sandboxID":"bad-tmp",`), 0o644); err != nil {
		t.Fatal(err)
	}

	records, err := store.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].State.SandboxID != valid.SandboxID {
		t.Fatalf("records=%#v, want only valid state", records)
	}
	if _, err := os.Stat(corruptTmpPath); err != nil {
		t.Fatalf("corrupt tmp state should remain for a later writer/recovery pass, stat err=%v", err)
	}
	if _, err := os.Stat(corruptSuccessPath); err != nil {
		t.Fatalf("corrupt non-tmp state should remain for inspection, stat err=%v", err)
	}
}

func TestStateStoreRejectsNewCreateWhenCommittedStateExists(t *testing.T) {
	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := testPersistedState("sandbox1")
	writeStateAs(t, store, state, StateFileSuccess)
	if err := store.WriteTmp(state); err == nil {
		t.Fatal("WriteTmp succeeded with existing success state, want error")
	}
	if err := store.CommitCreating(state.SandboxID); err == nil {
		t.Fatal("CommitCreating succeeded with existing success state, want error")
	}
}

func TestStateStoreLoadAnyReturnsFirstLifecycleFile(t *testing.T) {
	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := testPersistedState("sandbox1")
	writeStateAs(t, store, state, StateFileCreating)

	record, err := store.LoadAny("sandbox1")
	if err != nil {
		t.Fatal(err)
	}
	if record.Kind != StateFileCreating || record.State.SandboxID != "sandbox1" {
		t.Fatalf("record = %#v", record)
	}

	_, err = store.LoadAny("missing")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadAny missing err = %v", err)
	}
}

func TestStateStoreRecordFingerprintFencesReplacement(t *testing.T) {
	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := testPersistedState("sandbox-fingerprint")
	writeStateAs(t, store, state, StateFileDeleting)
	record, err := store.LoadAny(state.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Path == "" || record.Fingerprint == "" {
		t.Fatalf("record lacks exact identity: %#v", record)
	}
	if current, err := store.RecordCurrent(record); err != nil || !current {
		t.Fatalf("fresh record current=%v err=%v", current, err)
	}

	replacement := testPersistedState(state.SandboxID)
	replacement.NetworkHandle = "replacement-generation"
	writeStateAs(t, store, replacement, StateFileDeleting)
	if current, err := store.RecordCurrent(record); err != nil || current {
		t.Fatalf("replaced record current=%v err=%v, want false", current, err)
	}
	if deleted, err := store.DeleteRecordIfCurrent(record); err != nil || deleted {
		t.Fatalf("stale record deleted=%v err=%v, want no-op", deleted, err)
	}
	if !store.Exists(state.SandboxID, StateFileDeleting) {
		t.Fatal("replacement state was deleted by stale record")
	}
}

func TestStateStoreScanRejectsFilenamePayloadSandboxMismatch(t *testing.T) {
	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.path("filename-owner", StateFileDeleting)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := testPersistedState("payload-owner").MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Scan(); err == nil {
		t.Fatal("Scan accepted filename/payload sandbox mismatch")
	}
}

func testPersistedState(sandboxID string) *persistedState {
	return &persistedState{
		SandboxID:     sandboxID,
		NetworkHandle: sandboxID,
		TapName:       "z10.0.0.2",
		TapIfIndex:    12,
		SandboxIP:     "10.0.0.2",
		PortMappings: []PortMapping{{
			Protocol:      "tcp",
			HostIP:        "127.0.0.1",
			HostPort:      20080,
			ContainerPort: 80,
		}},
		PersistMetadata: map[string]string{"host_tap_name": "z10.0.0.2"},
	}
}

func writeStateAs(t *testing.T, store *stateStore, state *persistedState, kind StateFileKind) {
	t.Helper()
	path, err := store.path(state.SandboxID, kind)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := state.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertStateFile(t *testing.T, store *stateStore, sandboxID string, kind StateFileKind, wantExists bool) {
	t.Helper()
	path, err := store.path(sandboxID, kind)
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if exists != wantExists {
		t.Fatalf("exists(%s) = %t, want %t", filepath.Base(path), exists, wantExists)
	}
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode(%s) = %#o, want %#o", path, got, want)
	}
}

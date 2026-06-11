// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateStoreSaveLoadDelete(t *testing.T) {
	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("newStateStore error=%v", err)
	}

	state := &persistedState{
		SandboxID:     "sb-1",
		NetworkHandle: "sb-1",
		TapName:       "z192.168.0.10",
		TapIfIndex:    42,
		SandboxIP:     "192.168.0.10",
		Interfaces: []Interface{{
			Name:    "z192.168.0.10",
			MAC:     "20:90:6f:fc:fc:fc",
			MTU:     1500,
			IPs:     []string{"169.254.68.6/30"},
			Gateway: "169.254.68.5",
		}},
		PortMappings: []PortMapping{{
			Protocol:      "tcp",
			HostIP:        "127.0.0.1",
			HostPort:      65000,
			ContainerPort: 80,
		}},
		CubeNetworkConfig: &CubeNetworkConfig{
			AllowInternetAccess: boolPtr(true),
			AllowOut:            []string{"10.0.0.0/8"},
		},
		PersistMetadata: map[string]string{
			"sandbox_ip":    "192.168.0.10",
			"host_tap_name": "z192.168.0.10",
		},
	}
	if err := store.Save(state); err != nil {
		t.Fatalf("Save error=%v", err)
	}

	loaded, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll error=%v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("LoadAll len=%d, want=1", len(loaded))
	}
	if loaded[0].SandboxIP != state.SandboxIP {
		t.Fatalf("SandboxIP=%q, want=%q", loaded[0].SandboxIP, state.SandboxIP)
	}
	if loaded[0].CubeNetworkConfig == nil || loaded[0].CubeNetworkConfig.AllowInternetAccess == nil || *loaded[0].CubeNetworkConfig.AllowInternetAccess != *state.CubeNetworkConfig.AllowInternetAccess {
		t.Fatalf("CubeNetworkConfig=%+v, want AllowInternetAccess=%v", loaded[0].CubeNetworkConfig, state.CubeNetworkConfig.AllowInternetAccess)
	}

	if err := store.Delete(state.SandboxID); err != nil {
		t.Fatalf("Delete error=%v", err)
	}
	if err := store.Delete(state.SandboxID); err != nil {
		t.Fatalf("Delete second time error=%v", err)
	}
	loaded, err = store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll after delete error=%v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("LoadAll after delete len=%d, want=0", len(loaded))
	}
}

// TestStateStoreSaveDualWritesLegacyKey ensures that Save emits both
// `cubeNetworkConfig` (new) and `cubevsContext` (legacy) keys with the
// same payload, so a rollback to a pre-rename binary keeps reading state.
func TestStateStoreSaveDualWritesLegacyKey(t *testing.T) {
	store, err := newStateStore(t.TempDir())
	if err != nil {
		t.Fatalf("newStateStore error=%v", err)
	}
	state := &persistedState{
		SandboxID: "sb-dual",
		CubeNetworkConfig: &CubeNetworkConfig{
			AllowInternetAccess: boolPtr(false),
			AllowOut:            []string{"10.0.0.0/8"},
		},
	}
	if err := store.Save(state); err != nil {
		t.Fatalf("Save error=%v", err)
	}

	raw, err := os.ReadFile(filepath.Join(store.dir, "sb-dual.json"))
	if err != nil {
		t.Fatalf("ReadFile error=%v", err)
	}
	var blob map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blob); err != nil {
		t.Fatalf("Unmarshal error=%v", err)
	}
	newKey, hasNew := blob["cubeNetworkConfig"]
	legacyKey, hasLegacy := blob["cubevsContext"]
	if !hasNew || !hasLegacy {
		t.Fatalf("expected both cubeNetworkConfig and cubevsContext keys, got new=%v legacy=%v", hasNew, hasLegacy)
	}
	if string(newKey) != string(legacyKey) {
		t.Fatalf("dual-write payloads differ:\n new=%s\n legacy=%s", newKey, legacyKey)
	}
}

// TestStateStoreLoadAcceptsLegacyKey verifies that a state file written by a
// pre-rename binary (only cubevsContext key) is still readable.
func TestStateStoreLoadAcceptsLegacyKey(t *testing.T) {
	dir := t.TempDir()
	legacy := `{
		"sandboxID": "sb-legacy",
		"sandboxIP": "192.168.0.5",
		"cubevsContext": {"allowInternetAccess": false, "allowOut": ["10.0.0.0/8"]}
	}`
	if err := os.WriteFile(filepath.Join(dir, "sb-legacy.json"), []byte(legacy), 0o644); err != nil {
		t.Fatalf("WriteFile error=%v", err)
	}
	store, err := newStateStore(dir)
	if err != nil {
		t.Fatalf("newStateStore error=%v", err)
	}
	loaded, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll error=%v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("LoadAll len=%d, want 1", len(loaded))
	}
	cfg := loaded[0].CubeNetworkConfig
	if cfg == nil {
		t.Fatal("expected CubeNetworkConfig populated from legacy cubevsContext key")
	}
	if cfg.AllowInternetAccess == nil || *cfg.AllowInternetAccess {
		t.Fatalf("AllowInternetAccess=%v, want false", cfg.AllowInternetAccess)
	}
	if len(cfg.AllowOut) != 1 || cfg.AllowOut[0] != "10.0.0.0/8" {
		t.Fatalf("AllowOut=%v", cfg.AllowOut)
	}
}

// TestStateStoreLoadPrefersNewKey verifies that if a state file ever ends up
// containing both keys with different payloads (e.g. hand-edited), the new
// key wins.
func TestStateStoreLoadPrefersNewKey(t *testing.T) {
	dir := t.TempDir()
	body := strings.NewReplacer("\n", "", "\t", "").Replace(`{
		"sandboxID": "sb-both",
		"cubeNetworkConfig": {"allowInternetAccess": true},
		"cubevsContext":     {"allowInternetAccess": false}
	}`)
	if err := os.WriteFile(filepath.Join(dir, "sb-both.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile error=%v", err)
	}
	store, err := newStateStore(dir)
	if err != nil {
		t.Fatalf("newStateStore error=%v", err)
	}
	loaded, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll error=%v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("len=%d, want 1", len(loaded))
	}
	cfg := loaded[0].CubeNetworkConfig
	if cfg == nil || cfg.AllowInternetAccess == nil || !*cfg.AllowInternetAccess {
		t.Fatalf("AllowInternetAccess=%+v, want new key (true)", cfg)
	}
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import "testing"

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
			MTU:     1300,
			IPs:     []string{"169.254.68.6/30"},
			Gateway: "169.254.68.5",
		}},
		PortMappings: []PortMapping{{
			Protocol:      "tcp",
			HostIP:        "127.0.0.1",
			HostPort:      65000,
			ContainerPort: 80,
		}},
		CubeVSContext: &CubeVSContext{
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
	if loaded[0].CubeVSContext == nil || loaded[0].CubeVSContext.AllowInternetAccess == nil || *loaded[0].CubeVSContext.AllowInternetAccess != *state.CubeVSContext.AllowInternetAccess {
		t.Fatalf("CubeVSContext=%+v, want AllowInternetAccess=%v", loaded[0].CubeVSContext, state.CubeVSContext.AllowInternetAccess)
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

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodemeta

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func sampleHostFacts() *HostFacts {
	return &HostFacts{
		CPUVendor:             "GenuineIntel",
		CPUModel:              "Xeon 8255C",
		CPUIDHash:             "sha256:cpu",
		HostKernelRelease:     "5.15.0",
		HostKernelFingerprint: "sha256:kernel",
		KVMAPIVersion:         12,
	}
}

func TestHostFactsIsZero(t *testing.T) {
	if !(*HostFacts)(nil).IsZero() {
		t.Errorf("nil HostFacts must be zero")
	}
	if !(&HostFacts{}).IsZero() {
		t.Errorf("empty HostFacts must be zero")
	}
	// Each individual field must lift the value out of the zero set.
	for name, f := range map[string]*HostFacts{
		"vendor":      {CPUVendor: "x"},
		"model":       {CPUModel: "x"},
		"cpuid":       {CPUIDHash: "x"},
		"release":     {HostKernelRelease: "x"},
		"fingerprint": {HostKernelFingerprint: "x"},
		"kvm":         {KVMAPIVersion: 12},
	} {
		if f.IsZero() {
			t.Errorf("HostFacts with %s set must not be zero", name)
		}
	}
	// KVMModuleScanned is a transient collection signal, not a fact: a report
	// carrying only Scanned=true must still count as zero.
	if !(&HostFacts{KVMModuleScanned: true}).IsZero() {
		t.Errorf("HostFacts with only KVMModuleScanned set must be zero")
	}
}

func TestMarshalUnmarshalHostFacts_RoundTrip(t *testing.T) {
	in := sampleHostFacts()
	raw := marshalHostFacts(in)
	if raw == "" {
		t.Fatalf("non-zero facts must marshal to non-empty json")
	}
	out := unmarshalHostFacts(raw)
	if out == nil {
		t.Fatalf("round-trip produced nil")
	}
	if *out != *in {
		t.Errorf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestMarshalHostFacts_ZeroIsEmpty(t *testing.T) {
	if got := marshalHostFacts(nil); got != "" {
		t.Errorf("nil facts must marshal to empty string, got %q", got)
	}
	if got := marshalHostFacts(&HostFacts{}); got != "" {
		t.Errorf("empty facts must marshal to empty string, got %q", got)
	}
}

func TestRestoreMatchFactsJSON_OnlyCPUIDAndKernel(t *testing.T) {
	raw := RestoreMatchFactsJSON(sampleHostFacts())
	if raw == "" {
		t.Fatal("expected slim json")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want only cpuid_hash and host_kernel_release, got %v", got)
	}
	if got["cpuid_hash"] != "sha256:cpu" {
		t.Errorf("cpuid_hash=%v", got["cpuid_hash"])
	}
	if got["host_kernel_release"] != "5.15.0" {
		t.Errorf("host_kernel_release=%v", got["host_kernel_release"])
	}
}

func TestRestoreMatchFactsJSON_Empty(t *testing.T) {
	if got := RestoreMatchFactsJSON(nil); got != "" {
		t.Errorf("nil: got %q", got)
	}
	if got := RestoreMatchFactsJSON(&HostFacts{CPUVendor: "GenuineIntel"}); got != "" {
		t.Errorf("vendor-only must not freeze, got %q", got)
	}
}

func TestUnmarshalHostFacts_Degrades(t *testing.T) {
	if f := unmarshalHostFacts(""); f != nil {
		t.Errorf("empty string must unmarshal to nil, got %+v", f)
	}
	if f := unmarshalHostFacts("   "); f != nil {
		t.Errorf("blank string must unmarshal to nil, got %+v", f)
	}
	if f := unmarshalHostFacts("{}"); f != nil {
		t.Errorf("zero-value json must unmarshal to nil, got %+v", f)
	}
	if f := unmarshalHostFacts("not-json"); f != nil {
		t.Errorf("invalid json must unmarshal to nil, got %+v", f)
	}
}

func TestHostFactsEqual(t *testing.T) {
	a := sampleHostFacts()
	b := sampleHostFacts()
	if !hostFactsEqual(a, b) {
		t.Errorf("identical facts must be equal")
	}
	if !hostFactsEqual(nil, nil) {
		t.Errorf("two nils must be equal")
	}
	if hostFactsEqual(a, nil) || hostFactsEqual(nil, a) {
		t.Errorf("nil vs non-nil must not be equal")
	}
	b.HostKernelFingerprint = "sha256:changed"
	if hostFactsEqual(a, b) {
		t.Errorf("differing fingerprint must not be equal")
	}
}

// GetNodeHostFacts must mirror GetNodeComponentVersions gating: only a healthy,
// non-stale node that actually reported facts yields (facts, true).
func TestGetNodeHostFacts_Gating(t *testing.T) {
	orig := global
	t.Cleanup(func() { global = orig })

	now := time.Now()
	fresh := now.Add(-time.Second)
	stale := now.Add(-(healthTimeout() + time.Second))

	global = &service{nodes: map[string]*NodeSnapshot{
		"healthy": {
			NodeID:        "healthy",
			ReportedReady: true,
			HeartbeatTime: fresh,
			HostFacts:     sampleHostFacts(),
		},
		"stale": {
			NodeID:        "stale",
			ReportedReady: true,
			HeartbeatTime: stale,
			HostFacts:     sampleHostFacts(),
		},
		"no-facts": {
			NodeID:        "no-facts",
			ReportedReady: true,
			HeartbeatTime: fresh,
		},
	}}

	t.Run("healthy node with facts", func(t *testing.T) {
		got, ok := GetNodeHostFacts(context.Background(), "healthy")
		if !ok {
			t.Fatalf("healthy node with facts must return ok=true")
		}
		if got == nil || got.HostKernelFingerprint != "sha256:kernel" {
			t.Errorf("unexpected facts: %+v", got)
		}
		// Must be a clone, not the cached pointer.
		if got == global.nodes["healthy"].HostFacts {
			t.Errorf("returned facts must be a defensive copy, not the cached pointer")
		}
	})

	t.Run("unknown node", func(t *testing.T) {
		if _, ok := GetNodeHostFacts(context.Background(), "missing"); ok {
			t.Errorf("unknown node must return ok=false")
		}
	})

	t.Run("empty node id", func(t *testing.T) {
		if _, ok := GetNodeHostFacts(context.Background(), "  "); ok {
			t.Errorf("empty node id must return ok=false")
		}
	})

	t.Run("stale heartbeat", func(t *testing.T) {
		if _, ok := GetNodeHostFacts(context.Background(), "stale"); ok {
			t.Errorf("stale node must return ok=false")
		}
	})

	t.Run("healthy but no facts reported", func(t *testing.T) {
		if _, ok := GetNodeHostFacts(context.Background(), "no-facts"); ok {
			t.Errorf("node without host facts must return ok=false")
		}
	})
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package nodemeta

import (
	"encoding/json"
	"testing"
)

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

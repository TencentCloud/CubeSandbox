// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
)

func intelFacts() *nodemeta.HostFacts {
	return &nodemeta.HostFacts{
		CPUVendor:             "GenuineIntel",
		CPUModel:              "Xeon 8255C",
		CPUIDHash:             "sha256:intel",
		HostKernelRelease:     "5.15.0",
		HostKernelFingerprint: "sha256:kernel-a",
		KVMAPIVersion:         12,
		KVMModuleFingerprint:  "sha256:kvmmod-a",
		KVMModuleTaint:        "",
	}
}

func TestBuildHostFactDimensions_Identical(t *testing.T) {
	dims := buildHostFactDimensions(intelFacts(), intelFacts())
	if !allRequiredDimensionsMatch(dims) {
		t.Fatalf("identical hosts must be compatible, dims=%+v", dims)
	}
	for _, d := range dims {
		if !d.Required {
			t.Errorf("host-fact dimension %q must be required", d.Name)
		}
	}
}

func TestBuildHostFactDimensions_Mismatch(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(f *nodemeta.HostFacts)
		wantDim string
	}{
		{"cpuid", func(f *nodemeta.HostFacts) { f.CPUIDHash = "sha256:other" }, "cpuid_hash"},
		{"kernel_fingerprint", func(f *nodemeta.HostFacts) { f.HostKernelFingerprint = "sha256:other-kernel" }, "host_kernel_fingerprint"},
		{"kvm", func(f *nodemeta.HostFacts) { f.KVMAPIVersion = 13 }, "kvm_api_version"},
		{"kvm_module", func(f *nodemeta.HostFacts) { f.KVMModuleFingerprint = "sha256:other-kvmmod" }, "kvm_module_fingerprint"},
		{"vendor", func(f *nodemeta.HostFacts) { f.CPUVendor = "AuthenticAMD" }, "cpu_vendor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := intelFacts()
			tc.mutate(target)
			dims := buildHostFactDimensions(intelFacts(), target)
			if allRequiredDimensionsMatch(dims) {
				t.Fatalf("mismatch on %s must be incompatible", tc.name)
			}
			var found bool
			for _, d := range dims {
				if d.Name == tc.wantDim {
					found = true
					if d.Match {
						t.Errorf("dimension %q should not match", tc.wantDim)
					}
				}
			}
			if !found {
				t.Errorf("expected dimension %q not present", tc.wantDim)
			}
		})
	}
}

// A snapshot frozen before the fingerprint field existed must still compare on
// the bare kernel release, and a release mismatch must flip the verdict.
func TestBuildHostFactDimensions_KernelReleaseFallback(t *testing.T) {
	origin := intelFacts()
	origin.HostKernelFingerprint = ""
	target := intelFacts()
	target.HostKernelFingerprint = ""
	target.HostKernelRelease = "6.1.0"

	dims := buildHostFactDimensions(origin, target)
	if allRequiredDimensionsMatch(dims) {
		t.Fatalf("release mismatch (no fingerprint) must be incompatible")
	}
	var found bool
	for _, d := range dims {
		if d.Name == "host_kernel_fingerprint" {
			found = true
			if d.Match {
				t.Errorf("kernel dimension should not match on release fallback")
			}
			if d.Origin != "5.15.0" || d.Target != "6.1.0" {
				t.Errorf("fallback must compare bare release, got origin=%q target=%q", d.Origin, d.Target)
			}
		}
	}
	if !found {
		t.Errorf("host_kernel_fingerprint dimension missing")
	}
}

// The KVM-module taint gate is an integrity check on the *live target* node: a
// tainted target blocks restore even when both hosts present the same taint (two
// equally-tampered hosts are not "compatible"). An origin-only taint is frozen at
// snapshot-create time and cannot be re-verified, so it is reported
// informationally (non-blocking) — making it blocking would permanently mark the
// snapshot incompatible with every node including the origin itself, and would
// break same-node rollback on fleets that legitimately run out-of-tree/unsigned
// kvm.ko.
func TestBuildHostFactDimensions_KVMModuleTaintGate(t *testing.T) {
	cases := []struct {
		name         string
		originTaint  string
		targetTaint  string
		wantDimology bool // true if kvm_module_taint dimension should be present
		wantBlocked  bool // true if the taint dimension must be Required (blocking)
	}{
		{"both clean", "", "", false, false},
		{"target tainted", "", "E", true, true},
		{"origin tainted only", "O", "", true, false},
		{"both same taint", "E", "E", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			origin := intelFacts()
			origin.KVMModuleTaint = tc.originTaint
			target := intelFacts()
			target.KVMModuleTaint = tc.targetTaint

			dims := buildHostFactDimensions(origin, target)
			var taintDim *RestoreCompatDimension
			for i := range dims {
				if dims[i].Name == "kvm_module_taint" {
					taintDim = &dims[i]
				}
			}
			if tc.wantDimology != (taintDim != nil) {
				t.Fatalf("presence of kvm_module_taint = %v, want %v", taintDim != nil, tc.wantDimology)
			}
			if tc.wantBlocked && allRequiredDimensionsMatch(dims) {
				t.Errorf("live target taint (origin=%q target=%q) must block restore", tc.originTaint, tc.targetTaint)
			}
			if !tc.wantBlocked && !allRequiredDimensionsMatch(dims) {
				t.Errorf("a clean or origin-only taint must not block restore (origin=%q target=%q)", tc.originTaint, tc.targetTaint)
			}
			if taintDim != nil {
				if taintDim.Match {
					t.Errorf("taint dimension must never report Match, got %+v", *taintDim)
				}
				if taintDim.Required != tc.wantBlocked {
					t.Errorf("taint dimension Required = %v, want %v", taintDim.Required, tc.wantBlocked)
				}
			}
		})
	}
}

func TestBuildGuestDimensions_Informational(t *testing.T) {
	// A guest-image mismatch is informational and must NOT flip the verdict.
	target := map[string]string{"guest-image": "v2", "cube-agent": "a1"}
	dims := buildGuestDimensions("v1", "a1", target)
	if len(dims) != 2 {
		t.Fatalf("want 2 guest dims, got %d", len(dims))
	}
	for _, d := range dims {
		if d.Required {
			t.Errorf("guest dimension %q must be informational (Required=false)", d.Name)
		}
	}
	if !allRequiredDimensionsMatch(dims) {
		t.Errorf("informational mismatch must not affect verdict")
	}
}

func TestBuildGuestDimensions_UnknownTarget(t *testing.T) {
	if dims := buildGuestDimensions("v1", "a1", nil); dims != nil {
		t.Errorf("unknown target versions should yield no guest dims, got %+v", dims)
	}
}

func TestParseFrozenHostFacts(t *testing.T) {
	if f := parseFrozenHostFacts(""); f != nil {
		t.Errorf("empty json must parse to nil")
	}
	if f := parseFrozenHostFacts("{}"); f != nil {
		t.Errorf("zero-value facts must parse to nil")
	}
	if f := parseFrozenHostFacts("not-json"); f != nil {
		t.Errorf("invalid json must parse to nil")
	}
	f := parseFrozenHostFacts(`{"cpu_vendor":"GenuineIntel","cpuid_hash":"sha256:x"}`)
	if f == nil || f.CPUVendor != "GenuineIntel" || f.CPUIDHash != "sha256:x" {
		t.Errorf("valid json failed to parse: %+v", f)
	}
}

func TestReplicaGuestVersions(t *testing.T) {
	info := &SnapshotInfo{Replicas: []ReplicaStatus{
		{GuestImageVersion: "", AgentVersion: ""},
		{GuestImageVersion: "gi-1", AgentVersion: "ag-1"},
	}}
	gi, ag := replicaGuestVersions(info)
	if gi != "gi-1" || ag != "ag-1" {
		t.Errorf("replicaGuestVersions = %q, %q", gi, ag)
	}
}

func TestKVMVersionString(t *testing.T) {
	if kvmVersionString(0) != "" {
		t.Errorf("0 must map to empty string")
	}
	if kvmVersionString(12) != "12" {
		t.Errorf("12 must map to \"12\"")
	}
}

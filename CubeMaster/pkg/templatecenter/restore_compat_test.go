// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
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

// The v1 policy makes ONLY cpuid_hash and host_kernel_release required; every
// other host-fact dimension is informational.
func TestBuildHostFactDimensions_Identical(t *testing.T) {
	dims := buildHostFactDimensions(intelFacts(), intelFacts())
	if !allRequiredDimensionsMatch(dims) {
		t.Fatalf("identical hosts must be compatible, dims=%+v", dims)
	}
	requiredByName := map[string]bool{}
	for _, d := range dims {
		requiredByName[d.Name] = d.Required
	}
	wantRequired := map[string]bool{"cpuid_hash": true, "host_kernel_release": true}
	for name, required := range requiredByName {
		if wantRequired[name] != required {
			t.Errorf("dimension %q Required=%v, want %v", name, required, wantRequired[name])
		}
	}
	for name := range wantRequired {
		if _, ok := requiredByName[name]; !ok {
			t.Errorf("expected required dimension %q missing", name)
		}
	}
}

// A mismatch on a required key (cpuid_hash / host_kernel_release) must block
// restore; a mismatch on any demoted (informational) fact must NOT.
func TestBuildHostFactDimensions_Mismatch(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(f *nodemeta.HostFacts)
		wantDim     string
		wantBlocked bool
	}{
		{"cpuid", func(f *nodemeta.HostFacts) { f.CPUIDHash = "sha256:other" }, "cpuid_hash", true},
		{"kernel_release", func(f *nodemeta.HostFacts) { f.HostKernelRelease = "6.1.0" }, "host_kernel_release", true},
		{"kernel_fingerprint", func(f *nodemeta.HostFacts) { f.HostKernelFingerprint = "sha256:other-kernel" }, "host_kernel_fingerprint", false},
		{"kvm", func(f *nodemeta.HostFacts) { f.KVMAPIVersion = 13 }, "kvm_api_version", false},
		{"kvm_module", func(f *nodemeta.HostFacts) { f.KVMModuleFingerprint = "sha256:other-kvmmod" }, "kvm_module_fingerprint", false},
		{"vendor", func(f *nodemeta.HostFacts) { f.CPUVendor = "AuthenticAMD" }, "cpu_vendor", false},
		{"model", func(f *nodemeta.HostFacts) { f.CPUModel = "EPYC 7K62" }, "cpu_model", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := intelFacts()
			tc.mutate(target)
			dims := buildHostFactDimensions(intelFacts(), target)
			if got := !allRequiredDimensionsMatch(dims); got != tc.wantBlocked {
				t.Fatalf("mismatch on %s blocked=%v, want %v", tc.name, got, tc.wantBlocked)
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

// A required dimension whose signal is missing on BOTH sides must fail closed,
// not pass vacuously (""=="" is "not verified", not "compatible"). Here both
// hosts lack a cpuid_hash, so the required cpuid dimension must block restore.
func TestBuildHostFactDimensions_BothEmptyRequiredFailsClosed(t *testing.T) {
	origin := intelFacts()
	origin.CPUIDHash = ""
	target := intelFacts()
	target.CPUIDHash = ""

	dims := buildHostFactDimensions(origin, target)
	if allRequiredDimensionsMatch(dims) {
		t.Fatalf("both-empty required cpuid_hash must fail closed")
	}
	for _, d := range dims {
		if d.Name == "cpuid_hash" && d.Match {
			t.Errorf("both-empty cpuid_hash must not report match:true")
		}
	}
}

// An informational dimension with no signal on either side must report
// match:false ("not verified"), never a vacuous match:true — the response must
// distinguish "verified equal" from "couldn't check either side".
func TestBuildHostFactDimensions_BothEmptyInformationalNotVerified(t *testing.T) {
	origin := intelFacts()
	origin.KVMAPIVersion = 0
	origin.KVMModuleFingerprint = ""
	target := intelFacts()
	target.KVMAPIVersion = 0
	target.KVMModuleFingerprint = ""

	dims := buildHostFactDimensions(origin, target)
	// Required keys still match, so the verdict stays compatible...
	if !allRequiredDimensionsMatch(dims) {
		t.Fatalf("required keys match; verdict should stay compatible")
	}
	// ...but the unverifiable KVM dimensions must not claim a match.
	for _, d := range dims {
		if (d.Name == "kvm_api_version" || d.Name == "kvm_module_fingerprint") && d.Match {
			t.Errorf("both-empty %q must report match:false (not verified)", d.Name)
		}
	}
}

// When either side lacks a folded fingerprint, the kernel-fingerprint dimension
// must fall back to comparing bare release on BOTH sides, so a mixed-version
// rolling upgrade with a matching release reports match:true — never a spurious
// hash-vs-release mismatch.
func TestBuildHostFactDimensions_KernelFingerprintFallback(t *testing.T) {
	origin := intelFacts() // has HostKernelFingerprint set, release 5.15.0
	target := intelFacts()
	target.HostKernelFingerprint = "" // old cubelet: release-only
	target.HostKernelRelease = "5.15.0"

	dims := buildHostFactDimensions(origin, target)
	var found bool
	for _, d := range dims {
		if d.Name == "host_kernel_fingerprint" {
			found = true
			if !d.Match {
				t.Errorf("matching release must fall back to match:true, got %+v", d)
			}
			if d.Origin != "5.15.0" || d.Target != "5.15.0" {
				t.Errorf("fallback must compare bare release on both sides, got origin=%q target=%q", d.Origin, d.Target)
			}
		}
	}
	if !found {
		t.Fatalf("host_kernel_fingerprint dimension missing")
	}

	// A genuine release mismatch under the fallback must report match:false.
	target.HostKernelRelease = "6.1.0"
	for _, d := range buildHostFactDimensions(origin, target) {
		if d.Name == "host_kernel_fingerprint" && d.Match {
			t.Errorf("differing release under fallback must not match")
		}
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

// stubCandidates installs a fake QueryHostFactCandidates that emulates the real
// DB seam: when matchAll is false it returns only nodes whose required equality
// keys (cpuid_hash, host_kernel_release) equal the requested keys — mirroring
// the indexed SELECT. Unhealthy / no-facts nodes are assumed already excluded by
// the query, so pass only nodes that would survive the join. The taint gate is
// deliberately NOT emulated here: it must be applied in-app by listCompatibleNodes.
func stubCandidates(t *testing.T, all []*nodemeta.CandidateNode, err error) {
	t.Helper()
	orig := queryCandidatesFn
	t.Cleanup(func() { queryCandidatesFn = orig })
	queryCandidatesFn = func(_ context.Context, cpuidHash, kernelRelease string, matchAll bool) ([]*nodemeta.CandidateNode, error) {
		if err != nil {
			return nil, err
		}
		if matchAll {
			return all, nil
		}
		out := make([]*nodemeta.CandidateNode, 0, len(all))
		for _, c := range all {
			if c.HostFacts != nil && c.HostFacts.CPUIDHash == cpuidHash && c.HostFacts.HostKernelRelease == kernelRelease {
				out = append(out, c)
			}
		}
		return out, nil
	}
}

func TestListCompatibleNodesForFactors_FiltersByDefault(t *testing.T) {
	match := intelFacts()
	mismatch := intelFacts()
	mismatch.CPUIDHash = "sha256:other"
	// Tainted node shares the required keys (so the SQL predicate returns it) but
	// must still be rejected in-app by the kvm_module_taint gate.
	tainted := intelFacts()
	tainted.KVMModuleTaint = "E"

	stubCandidates(t, []*nodemeta.CandidateNode{
		{NodeID: "ok", HostIP: "10.0.0.1", HostFacts: match},
		{NodeID: "cpuid-mismatch", HostFacts: mismatch},
		{NodeID: "tainted", HostFacts: tainted},
	}, nil)

	res, err := ListCompatibleNodesForFactors(context.Background(), intelFacts(), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Nodes) != 1 {
		t.Fatalf("want 1 matching node, got %d: %+v", len(res.Nodes), res.Nodes)
	}
	if res.Nodes[0].NodeID != "ok" || !res.Nodes[0].Compatible || res.Nodes[0].NodeIP != "10.0.0.1" {
		t.Errorf("unexpected node: %+v", res.Nodes[0])
	}
}

// Bare-factor results must carry the partial-policy warning: the caller cannot
// supply the origin kvm_module_taint, so the taint gate is not evaluated and the
// verdict is a subset of the by-snapshot policy. By-snapshot results must not.
func TestListCompatibleNodesForFactors_CarriesPartialPolicyWarning(t *testing.T) {
	stubCandidates(t, []*nodemeta.CandidateNode{
		{NodeID: "ok", HostFacts: intelFacts()},
	}, nil)

	res, err := ListCompatibleNodesForFactors(context.Background(), intelFacts(), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Warning != RestoreCompatWarnBareFactorPartialPolicy {
		t.Fatalf("bare-factor result must carry the partial-policy warning, got %q", res.Warning)
	}
}

func TestListCompatibleNodesForFactors_IncludeAllReturnsRejected(t *testing.T) {
	mismatch := intelFacts()
	mismatch.CPUIDHash = "sha256:other"
	stubCandidates(t, []*nodemeta.CandidateNode{
		{NodeID: "ok", HostFacts: intelFacts()},
		{NodeID: "bad", HostFacts: mismatch},
	}, nil)

	res, err := ListCompatibleNodesForFactors(context.Background(), intelFacts(), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Nodes) != 2 {
		t.Fatalf("include_all must return every evaluated node, got %d", len(res.Nodes))
	}
	byID := map[string]CompatibleNode{}
	for _, n := range res.Nodes {
		byID[n.NodeID] = n
	}
	if !byID["ok"].Compatible {
		t.Errorf("ok node should be compatible")
	}
	if byID["bad"].Compatible {
		t.Errorf("bad node should be incompatible")
	}
	if len(byID["bad"].Dimensions) == 0 {
		t.Errorf("rejected node must carry dimensions for diagnosis")
	}
}

// A node the SQL matched on the two required keys must still be rejected in-app
// when it carries a suspicious kvm_module_taint, so the aggregate verdict never
// drifts from single-node EvaluateSnapshotRestoreCompat.
func TestListCompatibleNodesForFactors_TaintGateAppliedInApp(t *testing.T) {
	tainted := intelFacts()
	tainted.KVMModuleTaint = "O"
	stubCandidates(t, []*nodemeta.CandidateNode{
		{NodeID: "tainted", HostFacts: tainted},
	}, nil)

	// Default mode: the tainted node shares the required keys but is filtered out.
	res, err := ListCompatibleNodesForFactors(context.Background(), intelFacts(), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Nodes) != 0 {
		t.Fatalf("tainted node must be rejected in-app, got %+v", res.Nodes)
	}

	// include_all mode: returned but reported incompatible with the taint dim.
	res, err = ListCompatibleNodesForFactors(context.Background(), intelFacts(), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Nodes) != 1 || res.Nodes[0].Compatible {
		t.Fatalf("tainted node must be reported incompatible: %+v", res.Nodes)
	}
	var sawTaint bool
	for _, d := range res.Nodes[0].Dimensions {
		if d.Name == "kvm_module_taint" && d.Required && !d.Match {
			sawTaint = true
		}
	}
	if !sawTaint {
		t.Errorf("expected a failing required kvm_module_taint dimension: %+v", res.Nodes[0].Dimensions)
	}
}

// Rejected nodes must carry a human-readable Reason summarising the verdict so a
// caller does not have to re-derive it from the dimensions; compatible nodes must
// leave it empty. A required equality mismatch names the dimension; the taint gate
// reads as a gate with the offending taint letters.
func TestListCompatibleNodesForFactors_PopulatesReason(t *testing.T) {
	mismatch := intelFacts()
	mismatch.CPUIDHash = "sha256:other"
	tainted := intelFacts()
	tainted.KVMModuleTaint = "O"
	stubCandidates(t, []*nodemeta.CandidateNode{
		{NodeID: "ok", HostFacts: intelFacts()},
		{NodeID: "cpuid-mismatch", HostFacts: mismatch},
		{NodeID: "tainted", HostFacts: tainted},
	}, nil)

	res, err := ListCompatibleNodesForFactors(context.Background(), intelFacts(), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	byID := map[string]CompatibleNode{}
	for _, n := range res.Nodes {
		byID[n.NodeID] = n
	}
	if r := byID["ok"].Reason; r != "" {
		t.Errorf("compatible node must have empty reason, got %q", r)
	}
	if r := byID["cpuid-mismatch"].Reason; r != "required dimension mismatch: cpuid_hash" {
		t.Errorf("cpuid mismatch reason = %q", r)
	}
	if r := byID["tainted"].Reason; r != "kvm_module_taint gate: target module tainted (O)" {
		t.Errorf("taint reason = %q", r)
	}
}

func TestListCompatibleNodesForFactors_NoFactors(t *testing.T) {
	if _, err := ListCompatibleNodesForFactors(context.Background(), nil, false); !errors.Is(err, ErrRestoreCompatNoFactors) {
		t.Errorf("nil factors err = %v, want ErrRestoreCompatNoFactors", err)
	}
	if _, err := ListCompatibleNodesForFactors(context.Background(), &nodemeta.HostFacts{}, false); !errors.Is(err, ErrRestoreCompatNoFactors) {
		t.Errorf("zero factors err = %v, want ErrRestoreCompatNoFactors", err)
	}
}

func TestListCompatibleNodesForFactors_ListError(t *testing.T) {
	wantErr := errors.New("registry down")
	stubCandidates(t, nil, wantErr)
	if _, err := ListCompatibleNodesForFactors(context.Background(), intelFacts(), false); !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}

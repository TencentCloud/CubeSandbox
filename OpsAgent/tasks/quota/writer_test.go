// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"strings"
	"testing"
)

const sampleConf = `common:
  common_timeout: "10s"
  # a comment that must survive merges
  default_dns_servers:
    - 119.29.29.29

host:
  scheduler_label: "default-cluster"
  quota:
    mcpu_limit: 0
    mem_limit: ""
    mvm_limit: 0
    creation_concurrent_num: 0
    # inline comment inside the quota section
    paused_resource_release_ratio: 0.0
  gc:
    code_expiration_time: "72h"

meta_server_config:
  meta_server_endpoint: "127.0.0.1:3010"
`

func TestMergeReplacesOwnedKeysOnly(t *testing.T) {
	spec := &Spec{MCpuLimit: 128000, MemLimit: "256Gi", MvmLimit: 500, CreationConcurrentNum: 32, PausedResourceReleaseRatio: 0.5}
	out, err := Merge(sampleConf, spec)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	for _, want := range []string{
		"    mcpu_limit: 128000",
		`    mem_limit: "256Gi"`,
		"    mvm_limit: 500",
		"    creation_concurrent_num: 32",
		"    paused_resource_release_ratio: 0.5",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("merged output missing %q:\n%s", want, out)
		}
	}
	// Unrelated content is preserved byte-for-byte.
	for _, keep := range []string{
		"  common_timeout: \"10s\"",
		"# a comment that must survive merges",
		"# inline comment inside the quota section",
		"    code_expiration_time: \"72h\"",
		"  meta_server_endpoint: \"127.0.0.1:3010\"",
		"  scheduler_label: \"default-cluster\"",
	} {
		if !strings.Contains(out, keep) {
			t.Fatalf("merged output lost %q:\n%s", keep, out)
		}
	}
}

func TestMergeInsertsMissingKeys(t *testing.T) {
	conf := strings.ReplaceAll(sampleConf, "    mvm_limit: 0\n", "")
	conf = strings.ReplaceAll(conf, "    creation_concurrent_num: 0\n", "")
	spec := &Spec{MvmLimit: 42, CreationConcurrentNum: 7}
	out, err := Merge(conf, spec)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !strings.Contains(out, "    mvm_limit: 42") || !strings.Contains(out, "    creation_concurrent_num: 7") {
		t.Fatalf("missing keys not inserted:\n%s", out)
	}
	// Inserted keys land inside the quota section, before gc.
	if strings.Index(out, "mvm_limit: 42") > strings.Index(out, "gc:") {
		t.Fatalf("inserted key escaped the quota section:\n%s", out)
	}
}

func TestMergeIsIdempotent(t *testing.T) {
	spec := &Spec{MCpuLimit: 96000, MemLimit: "128Gi"}
	once, err := Merge(sampleConf, spec)
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}
	twice, err := Merge(once, spec)
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if once != twice {
		t.Fatalf("merge not idempotent:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

func TestMatches(t *testing.T) {
	spec := &Spec{MCpuLimit: 128000, MemLimit: "256Gi"}
	merged, err := Merge(sampleConf, spec)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if ok, err := Matches(merged, spec); err != nil || !ok {
		t.Fatalf("merged content should match: ok=%v err=%v", ok, err)
	}
	if ok, _ := Matches(sampleConf, spec); ok {
		t.Fatal("original content should not match the spec")
	}
}

func TestValidate(t *testing.T) {
	phys := PhysicalTotals{CpuTotal: 16, MemMBTotal: 39799}
	cases := []struct {
		name    string
		spec    *Spec
		wantErr bool
	}{
		{"ok", &Spec{MCpuLimit: 128000, MemLimit: "64Gi"}, false},
		{"cpu at ceiling", &Spec{MCpuLimit: 16 * 1000 * 20}, false},
		{"cpu over ceiling", &Spec{MCpuLimit: 16 * 1000 * 21}, true},
		{"mem at ceiling", &Spec{MemLimit: "119397Mi"}, false},  // 39799MB * 3 exactly
		{"mem over ceiling", &Spec{MemLimit: "119398Mi"}, true}, // one MB over
		{"mem far over ceiling", &Spec{MemLimit: "512Gi"}, true},
		{"bad mem format", &Spec{MemLimit: "256 gib"}, true},
		{"negative mcpu", &Spec{MCpuLimit: -1}, true},
		{"ratio out of range", &Spec{PausedResourceReleaseRatio: 1.5}, true},
		{"zero spec is valid", &Spec{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate(phys)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestParseMemLimitMB(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"256Gi", 262144},
		{"262144Mi", 262144},
		{"1.5Gi", 1536},
		{"1024Ki", 1}, // rounds up
		{"1Ti", 1024 * 1024},
	}
	for _, tc := range cases {
		if got, err := parseMemLimitMB(tc.in); err != nil || got != tc.want {
			t.Errorf("parseMemLimitMB(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
	// Bare numbers and decimal suffixes are rejected: cubelet's
	// resource.ParseQuantity reads them as bytes, desyncing spec vs actual.
	for _, in := range []string{"abc", "262144", "1G", "512M", "100K"} {
		if _, err := parseMemLimitMB(in); err == nil {
			t.Errorf("parseMemLimitMB(%q) should be rejected", in)
		}
	}
}

func TestParseMemTotalKB(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    int64
		wantErr bool
	}{
		{"typical", "MemTotal:       16265044 kB\nMemFree: 1 kB\n", 16265044, false},
		{"with surrounding lines", "MemFree: 1 kB\nMemTotal: 4096 kB\nCached: 2 kB\n", 4096, false},
		{"missing", "MemFree: 1 kB\nCached: 2 kB\n", 0, true},
		{"malformed", "MemTotal: not-a-number kB\n", 0, true},
		{"no value", "MemTotal:\n", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMemTotalKB(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseMemTotalKB err=%v wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("parseMemTotalKB=%d want %d", got, tc.want)
			}
		})
	}
}

func TestMergeRejectsConfWithoutHostQuota(t *testing.T) {
	if _, err := Merge("common:\n  a: 1\n", &Spec{}); err == nil {
		t.Fatal("merge should fail without host.quota section")
	}
}

func TestMatchesMergeOnlyRatio(t *testing.T) {
	base := "host:\n  quota:\n    mcpu_limit: 16000\n    mem_limit: \"19672Mi\"\n    mvm_limit: 38\n    creation_concurrent_num: 0\n    paused_resource_release_ratio: 0.0\n"
	spec := &Spec{MCpuLimit: 99999, MemLimit: "1Gi", MvmLimit: 1, CreationConcurrentNum: 9, PausedResourceReleaseRatio: 0.5, OnlyRatio: true}

	// Only the ratio is compared and rewritten; the other keys stay intact.
	if ok, err := Matches(base, spec); err != nil || ok {
		t.Fatalf("Matches = (%v, %v), want (false, nil)", ok, err)
	}
	merged, err := Merge(base, spec)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if ok, err := Matches(merged, spec); err != nil || !ok {
		t.Fatalf("Matches(merged) = (%v, %v), want (true, nil)", ok, err)
	}
	for _, keep := range []string{"mcpu_limit: 16000", `"19672Mi"`, "mvm_limit: 38", "creation_concurrent_num: 0", "paused_resource_release_ratio: 0.5"} {
		if !strings.Contains(merged, keep) {
			t.Fatalf("merged lost %q:\n%s", keep, merged)
		}
	}
	if strings.Contains(merged, "99999") || strings.Contains(merged, "1Gi") {
		t.Fatalf("only-ratio merge must not touch resource keys:\n%s", merged)
	}
}

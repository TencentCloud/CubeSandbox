// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// TestUnmarshalTemplateImageJobRequestPreservesTemplateID guards against a
// regression where decoding an already-submitted job's RequestJSON snapshot
// silently minted a SECOND, different template_id (via
// normalizeTemplateImageRequest's unconditional regeneration), leaving the
// job's real template_id row (definition + replicas) orphaned while a
// different, freshly generated ID got used downstream instead. See
// remote_build_resume.go's resumeRemoteBuiltTemplateImageJob, which decodes
// the snapshot and must operate on the SAME template_id the job (and its
// replicas) were created under.
func TestUnmarshalTemplateImageJobRequestPreservesTemplateID(t *testing.T) {
	original := &types.CreateTemplateFromImageReq{
		TemplateID:        "tpl-original0000000000000000",
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
	}
	payload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal fail: %v", err)
	}

	got, err := unmarshalTemplateImageJobRequest(string(payload))
	if err != nil {
		t.Fatalf("unmarshalTemplateImageJobRequest failed: %v", err)
	}
	if got.TemplateID != original.TemplateID {
		t.Fatalf("template_id must be preserved across decode: got %q, want %q", got.TemplateID, original.TemplateID)
	}
}

func TestBuildTemplateSpecFingerprintIgnoresRuntimeOverrides(t *testing.T) {
	base := func(overrides *types.ContainerOverrides) *types.CreateTemplateFromImageReq {
		return &types.CreateTemplateFromImageReq{
			SourceImageRef:     "docker.io/library/nginx:latest",
			WritableLayerSize:  "20Gi",
			InstanceType:       "cubebox",
			NetworkType:        "tap",
			ContainerOverrides: overrides,
		}
	}
	const digest = "repo@sha256:aaa"

	fpNil := buildTemplateSpecFingerprint(base(nil), digest)
	cases := []struct {
		name      string
		overrides *types.ContainerOverrides
	}{
		{name: "empty", overrides: &types.ContainerOverrides{}},
		{name: "empty-annotations", overrides: &types.ContainerOverrides{Annotations: map[string]string{}}},
		{name: "resources-1c1g", overrides: &types.ContainerOverrides{Resources: &types.Resource{Cpu: "1000m", Mem: "1024Mi"}}},
		{name: "resources-4c8g", overrides: &types.ContainerOverrides{Resources: &types.Resource{Cpu: "4000m", Mem: "8192Mi"}}},
		{
			name: "all-runtime-overrides",
			overrides: &types.ContainerOverrides{
				Resources:    &types.Resource{Cpu: "2000m", Mem: "2048Mi"},
				Command:      []string{"/bin/sh"},
				Args:         []string{"-c", "sleep infinity"},
				WorkingDir:   "/workspace",
				Envs:         []*types.KeyValue{{Key: "FOO", Value: "bar"}},
				Probe:        &types.Probe{TimeoutMs: 30000},
				VolumeMounts: []*cubeboxv1.VolumeMounts{{Name: "data", ContainerPath: "/data"}},
				RLimit:       &types.RLimit{NoFile: 1024},
			},
		},
	}
	for _, tc := range cases {
		if got := buildTemplateSpecFingerprint(base(tc.overrides), digest); got != fpNil {
			t.Fatalf("%s should not affect fingerprint: got=%q want=%q", tc.name, got, fpNil)
		}
	}

	// Build-time/retained overrides must still affect the fingerprint and remain
	// insensitive to accompanying runtime-only overrides.
	uid := &types.Int64Value{Value: 1000}
	retained := []struct {
		name      string
		only      *types.ContainerOverrides
		withExtra *types.ContainerOverrides
	}{
		{
			name: "dns",
			only: &types.ContainerOverrides{DnsConfig: &types.DNSConfig{Servers: []string{"8.8.8.8"}}},
			withExtra: &types.ContainerOverrides{
				DnsConfig: &types.DNSConfig{Servers: []string{"8.8.8.8"}},
				Resources: &types.Resource{Cpu: "2000m", Mem: "2048Mi"},
				Command:   []string{"/bin/sh"},
			},
		},
		{
			name: "security-context",
			only: &types.ContainerOverrides{SecurityContext: &types.ContainerSecurityContext{RunAsUser: uid}},
			withExtra: &types.ContainerOverrides{
				SecurityContext: &types.ContainerSecurityContext{RunAsUser: uid},
				Envs:            []*types.KeyValue{{Key: "A", Value: "1"}},
			},
		},
		{
			name: "annotations",
			only: &types.ContainerOverrides{Annotations: map[string]string{"k": "v"}},
			withExtra: &types.ContainerOverrides{
				Annotations: map[string]string{"k": "v"},
				WorkingDir:  "/app",
			},
		},
	}
	for _, tc := range retained {
		fpOnly := buildTemplateSpecFingerprint(base(tc.only), digest)
		fpExtra := buildTemplateSpecFingerprint(base(tc.withExtra), digest)
		if fpOnly != fpExtra {
			t.Fatalf("%s: runtime overrides should not alter fingerprint: %q vs %q", tc.name, fpOnly, fpExtra)
		}
		if fpOnly == fpNil {
			t.Fatalf("%s must still affect fingerprint: %q", tc.name, fpOnly)
		}
	}
}

// TestFingerprintContainerOverridesClassifiesAllFields guards the allowlist in
// fingerprintContainerOverrides: adding a new field to types.ContainerOverrides
// fails this test until the author explicitly decides whether it affects the
// rootfs artifact (retained) or is runtime-only (excluded).
func TestFingerprintContainerOverridesClassifiesAllFields(t *testing.T) {
	retained := map[string]bool{
		"DnsConfig":       true,
		"SecurityContext": true,
		"Annotations":     true,
	}
	excluded := map[string]bool{
		"Command":      true,
		"Args":         true,
		"WorkingDir":   true,
		"Envs":         true,
		"Resources":    true,
		"Probe":        true,
		"VolumeMounts": true,
		"RLimit":       true,
	}

	typ := reflect.TypeOf(types.ContainerOverrides{})
	seen := make(map[string]bool, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		seen[name] = true
		if retained[name] == excluded[name] {
			t.Fatalf("types.ContainerOverrides.%s must be classified in exactly one of retained or excluded (update fingerprintContainerOverrides if it affects rootfs)", name)
		}
	}
	for name := range retained {
		if !seen[name] {
			t.Fatalf("stale retained field %q no longer exists on types.ContainerOverrides", name)
		}
	}
	for name := range excluded {
		if !seen[name] {
			t.Fatalf("stale excluded field %q no longer exists on types.ContainerOverrides", name)
		}
	}
}

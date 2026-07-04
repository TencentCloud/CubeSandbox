// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/image"
	"gorm.io/gorm"
)

func TestNormalizeTemplateImageRequestDefaults(t *testing.T) {

	req, err := normalizeTemplateImageRequest(&types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
	})
	if err != nil {
		t.Fatalf("normalizeTemplateImageRequest failed: %v", err)
	}
	if req.InstanceType != cubeboxv1.InstanceType_cubebox.String() {
		t.Fatalf("InstanceType=%q", req.InstanceType)
	}
	if req.NetworkType != cubeboxv1.NetworkType_tap.String() {
		t.Fatalf("NetworkType=%q", req.NetworkType)
	}
	if req.TemplateID == "" {
		t.Fatal("TemplateID should be generated when omitted")
	}
	if !strings.HasPrefix(req.TemplateID, "tpl-") {
		t.Fatalf("unexpected generated TemplateID: %q", req.TemplateID)
	}
}

func TestNormalizeTemplateImageRequestTrimsAndValidatesSourceImageRef(t *testing.T) {
	req, err := normalizeTemplateImageRequest(&types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "  registry.example.com:5000/ns/app:1.2.3  ",
		WritableLayerSize: "20Gi",
	})
	if err != nil {
		t.Fatalf("normalizeTemplateImageRequest failed: %v", err)
	}
	if req.SourceImageRef != "registry.example.com:5000/ns/app:1.2.3" {
		t.Fatalf("SourceImageRef=%q, want trimmed reference", req.SourceImageRef)
	}
}

func TestNormalizeTemplateImageRequestRejectsInvalidSourceImageRef(t *testing.T) {
	invalidRefs := []string{
		"",
		"--help",
		"-v",
		"docker://--help",
		"registry.example.com/image --authfile /etc/shadow",
		"registry.example.com/image\n--flag",
		"image;rm",
		"library/nginx:",
		"library/nginx@sha256:not-a-digest",
	}
	for _, imageRef := range invalidRefs {
		t.Run(imageRef, func(t *testing.T) {
			_, err := normalizeTemplateImageRequest(&types.CreateTemplateFromImageReq{
				Request:           &types.Request{RequestID: "req-1"},
				SourceImageRef:    imageRef,
				WritableLayerSize: "20Gi",
			})
			if err == nil || !strings.Contains(err.Error(), "source_image_ref") {
				t.Fatalf("normalizeTemplateImageRequest(%q) error=%v, want source_image_ref validation error", imageRef, err)
			}
		})
	}
}

func TestNormalizeTemplateImageRequestIgnoresProvidedTemplateID(t *testing.T) {

	req, err := normalizeTemplateImageRequest(&types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "custom-template",
		WritableLayerSize: "20Gi",
	})
	if err != nil {
		t.Fatalf("normalizeTemplateImageRequest failed: %v", err)
	}
	if req.TemplateID == "custom-template" {
		t.Fatal("provided TemplateID should be ignored")
	}
	if !strings.HasPrefix(req.TemplateID, "tpl-") {
		t.Fatalf("unexpected generated TemplateID: %q", req.TemplateID)
	}
}

func TestNormalizeTemplateImageRequestDropsDisabledIvshmemFlag(t *testing.T) {
	disabled := false
	req, err := normalizeTemplateImageRequest(&types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
		EnableIvshmem:     &disabled,
	})
	if err != nil {
		t.Fatalf("normalizeTemplateImageRequest failed: %v", err)
	}
	if req.EnableIvshmem != nil {
		t.Fatal("EnableIvshmem should be canonicalized to nil when false")
	}
}

func TestNormalizeTemplateImageRequestNormalizesExposedPorts(t *testing.T) {

	req, err := normalizeTemplateImageRequest(&types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
		ExposedPorts:      []int32{9000, 80, 8080, 9000},
	})
	if err != nil {
		t.Fatalf("normalizeTemplateImageRequest failed: %v", err)
	}
	want := []int32{80, 8080, 9000}
	if !reflect.DeepEqual(req.ExposedPorts, want) {
		t.Fatalf("ExposedPorts=%v, want %v", req.ExposedPorts, want)
	}
}

func TestNormalizeTemplateImageRequestAllowsEmptyExposedPortsWhenEnabled(t *testing.T) {

	req, err := normalizeTemplateImageRequest(&types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
	})
	if err != nil {
		t.Fatalf("normalizeTemplateImageRequest failed: %v", err)
	}
	if len(req.ExposedPorts) != 0 {
		t.Fatalf("expected empty exposed ports, got %v", req.ExposedPorts)
	}
}

func TestNormalizeTemplateImageRequestRejectsDomainAllowOutWithoutDenyAll(t *testing.T) {
	_, err := normalizeTemplateImageRequest(&types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
		CubeNetworkConfig: &types.CubeNetworkConfig{
			AllowOut: []string{"example.com"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "0.0.0.0/0") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNormalizeTemplateImageRequestAllowsDomainAllowOutWithDenyAll(t *testing.T) {
	_, err := normalizeTemplateImageRequest(&types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
		CubeNetworkConfig: &types.CubeNetworkConfig{
			AllowOut: []string{"*.example.com"},
			DenyOut:  []string{"0.0.0.0/0"},
		},
	})
	if err != nil {
		t.Fatalf("normalizeTemplateImageRequest failed: %v", err)
	}
}

func TestNormalizeTemplateImageRequestAllowsDomainAllowOutWithDisabledInternet(t *testing.T) {
	allowInternetAccess := false
	_, err := normalizeTemplateImageRequest(&types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
		CubeNetworkConfig: &types.CubeNetworkConfig{
			AllowInternetAccess: &allowInternetAccess,
			AllowOut:            []string{"example.com"},
		},
	})
	if err != nil {
		t.Fatalf("normalizeTemplateImageRequest failed: %v", err)
	}
}

func TestNormalizeTemplateImageRequestAllowsCIDRAllowOutWithoutDenyAll(t *testing.T) {
	_, err := normalizeTemplateImageRequest(&types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
		CubeNetworkConfig: &types.CubeNetworkConfig{
			AllowOut: []string{"203.0.113.0/24", "8.8.8.8"},
		},
	})
	if err != nil {
		t.Fatalf("normalizeTemplateImageRequest failed: %v", err)
	}
}

func TestNormalizeTemplateImageRequestRejectsTooManyCustomExposedPorts(t *testing.T) {

	_, err := normalizeTemplateImageRequest(&types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
		ExposedPorts:      []int32{9000, 9001, 9002, 9003},
	})
	if err == nil || !strings.Contains(err.Error(), "at most 3 custom exposed ports") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDefaultTemplateExposedPortsContainsOnly49983(t *testing.T) {
	got := defaultTemplateExposedPorts()
	want := map[int32]struct{}{49983: {}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaultTemplateExposedPorts()=%v, want %v", got, want)
	}
}

func TestCountCustomTemplateExposedPortsTreats49983AsReserved(t *testing.T) {
	if count := countCustomTemplateExposedPorts([]int32{49983, 9000}); count != 1 {
		t.Fatalf("countCustomTemplateExposedPorts([49983, 9000])=%d, want 1", count)
	}
	if count := countCustomTemplateExposedPorts([]int32{8080, 9000}); count != 2 {
		t.Fatalf("countCustomTemplateExposedPorts([8080, 9000])=%d, want 2", count)
	}
}

func TestNormalizeTemplateImageRequestTreatsOnlyCubeletDefaultsAsReserved(t *testing.T) {

	_, err := normalizeTemplateImageRequest(&types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
		ExposedPorts:      []int32{80, 9000, 9001, 9002},
	})
	if err == nil || !strings.Contains(err.Error(), "at most 3 custom exposed ports") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNormalizeRequestGeneratesTemplateIDWhenMissing(t *testing.T) {
	req, templateID, err := NormalizeRequest(&types.CreateCubeSandboxReq{
		Request: &types.Request{RequestID: "req-1"},
		Annotations: map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
		},
	})
	if err != nil {
		t.Fatalf("NormalizeRequest failed: %v", err)
	}
	if templateID == "" {
		t.Fatal("templateID should be generated")
	}
	if !strings.HasPrefix(templateID, "tpl-") {
		t.Fatalf("unexpected generated templateID: %q", templateID)
	}
	if got := req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID]; got != templateID {
		t.Fatalf("template annotation mismatch: %q", got)
	}
}

func TestNormalizeRequestRejectsDomainAllowOutWithoutDenyAll(t *testing.T) {
	_, _, err := NormalizeRequest(&types.CreateCubeSandboxReq{
		Request: &types.Request{RequestID: "req-1"},
		Annotations: map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
		},
		CubeNetworkConfig: &types.CubeNetworkConfig{
			AllowOut: []string{"example.com"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "0.0.0.0/0") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNormalizeRequestRejectsInvalidTemplateIDPrefix(t *testing.T) {
	tests := []string{
		"template-1",
		"custom-template",
		"sb-123",
		"op-123",
		"tpl-",
		"snap-",
		"tpl-   ",
		"snap-   ",
	}
	for _, templateID := range tests {
		t.Run(templateID, func(t *testing.T) {
			_, _, err := NormalizeRequest(&types.CreateCubeSandboxReq{
				Request: &types.Request{RequestID: "req-1"},
				Annotations: map[string]string{
					constants.CubeAnnotationAppSnapshotTemplateID:      templateID,
					constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
				},
			})
			if err == nil {
				t.Fatalf("expected invalid template ID error for %q", templateID)
			}
			if !strings.Contains(err.Error(), constants.CubeAnnotationAppSnapshotTemplateID) {
				t.Fatalf("error should include annotation key, got %v", err)
			}
		})
	}
}

func TestNormalizeRequestAcceptsTemplateAndSnapshotPrefixes(t *testing.T) {
	tests := []string{"tpl-existing", "snap-existing"}
	for _, templateID := range tests {
		t.Run(templateID, func(t *testing.T) {
			req, got, err := NormalizeRequest(&types.CreateCubeSandboxReq{
				Request: &types.Request{RequestID: "req-1"},
				Annotations: map[string]string{
					constants.CubeAnnotationAppSnapshotTemplateID:      templateID,
					constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
				},
			})
			if err != nil {
				t.Fatalf("NormalizeRequest failed: %v", err)
			}
			if got != templateID {
				t.Fatalf("templateID=%q, want %q", got, templateID)
			}
			if req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] != templateID {
				t.Fatalf("template annotation mismatch: %q", req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
			}
		})
	}
}

func TestBuildTemplateSpecFingerprintUsesDigest(t *testing.T) {

	req, err := normalizeTemplateImageRequest(&types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "template-1",
		WritableLayerSize: "20Gi",
	})
	if err != nil {
		t.Fatalf("normalizeTemplateImageRequest failed: %v", err)
	}
	fingerprintA := buildTemplateSpecFingerprint(req, "repo@sha256:aaa")
	fingerprintB := buildTemplateSpecFingerprint(req, "repo@sha256:bbb")
	if fingerprintA == "" || fingerprintB == "" {
		t.Fatalf("fingerprint should not be empty")
	}
	if fingerprintA == fingerprintB {
		t.Fatalf("fingerprint should change when digest changes")
	}
}

func TestBuildTemplateSpecFingerprintUsesExposedPorts(t *testing.T) {
	reqA := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "template-1",
		WritableLayerSize: "20Gi",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
		ExposedPorts:      []int32{8080},
	}
	reqB := &types.CreateTemplateFromImageReq{
		Request:           reqA.Request,
		SourceImageRef:    reqA.SourceImageRef,
		TemplateID:        reqA.TemplateID,
		WritableLayerSize: reqA.WritableLayerSize,
		InstanceType:      reqA.InstanceType,
		NetworkType:       reqA.NetworkType,
		ExposedPorts:      []int32{8080, 9000},
	}
	if gotA, gotB := buildTemplateSpecFingerprint(reqA, "repo@sha256:aaa"), buildTemplateSpecFingerprint(reqB, "repo@sha256:aaa"); gotA == gotB {
		t.Fatalf("fingerprint should change when exposed ports change")
	}
}

func TestBuildTemplateSpecFingerprintUsesDNSConfig(t *testing.T) {
	reqA := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "template-1",
		WritableLayerSize: "20Gi",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
		ContainerOverrides: &types.ContainerOverrides{
			DnsConfig: &types.DNSConfig{Servers: []string{"8.8.8.8"}},
		},
	}
	reqB := &types.CreateTemplateFromImageReq{
		Request:           reqA.Request,
		SourceImageRef:    reqA.SourceImageRef,
		TemplateID:        reqA.TemplateID,
		WritableLayerSize: reqA.WritableLayerSize,
		InstanceType:      reqA.InstanceType,
		NetworkType:       reqA.NetworkType,
		ContainerOverrides: &types.ContainerOverrides{
			DnsConfig: &types.DNSConfig{Servers: []string{"1.1.1.1"}},
		},
	}
	reqC := &types.CreateTemplateFromImageReq{
		Request:            reqA.Request,
		SourceImageRef:     reqA.SourceImageRef,
		TemplateID:         reqA.TemplateID,
		WritableLayerSize:  reqA.WritableLayerSize,
		InstanceType:       reqA.InstanceType,
		NetworkType:        reqA.NetworkType,
		ContainerOverrides: &types.ContainerOverrides{},
	}
	if gotA, gotB := buildTemplateSpecFingerprint(reqA, "repo@sha256:aaa"), buildTemplateSpecFingerprint(reqB, "repo@sha256:aaa"); gotA == gotB {
		t.Fatalf("fingerprint should change when DNS config changes")
	}
	if gotA, gotC := buildTemplateSpecFingerprint(reqA, "repo@sha256:aaa"), buildTemplateSpecFingerprint(reqC, "repo@sha256:aaa"); gotA == gotC {
		t.Fatalf("fingerprint should change when DNS config is removed")
	}
}

func TestBuildTemplateSpecFingerprintEmptyCAMatchesLegacy(t *testing.T) {
	// Critical invariant: an environment with no CubeEgress configured
	// must produce the SAME fingerprint as before the CA feature
	// existed. Otherwise every dev/test deployment would rebuild every
	// artifact on first run after upgrade, even though no CA was ever
	// involved. The "" CA fingerprint omitempty's out of the JSON.
	req := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "template-1",
		WritableLayerSize: "20Gi",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
	}
	legacy := buildTemplateSpecFingerprint(req, "repo@sha256:aaa")
	withEmptyCA := buildTemplateSpecFingerprintWithCA(req, "repo@sha256:aaa", "")
	if legacy != withEmptyCA {
		t.Fatalf("empty CA fingerprint must yield the legacy spec fingerprint:\n legacy=%s\n withEmptyCA=%s", legacy, withEmptyCA)
	}
}

func TestBuildTemplateSpecFingerprintWithCAChangesOnRotation(t *testing.T) {
	req := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "template-1",
		WritableLayerSize: "20Gi",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
	}
	a := buildTemplateSpecFingerprintWithCA(req, "repo@sha256:aaa", "fp-old")
	b := buildTemplateSpecFingerprintWithCA(req, "repo@sha256:aaa", "fp-new")
	if a == b {
		t.Fatal("CA fingerprint rotation must yield a different spec fingerprint, otherwise the artifact reuse cache would serve stale CAs")
	}
	// Same CA → same fingerprint (idempotent for identical inputs).
	again := buildTemplateSpecFingerprintWithCA(req, "repo@sha256:aaa", "fp-old")
	if a != again {
		t.Fatal("fingerprint should be deterministic for identical inputs")
	}
}

func TestNewCreateTemplateImageJobRecordPersistsRequestID(t *testing.T) {
	record := newCreateTemplateImageJobRecord(
		"job-1",
		&types.CreateTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-123"},
			TemplateID:        "tpl-1",
			SourceImageRef:    "docker.io/library/nginx:latest",
			WritableLayerSize: "20Gi",
			InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
			NetworkType:       cubeboxv1.NetworkType_tap.String(),
		},
		`{"template_id":"tpl-1"}`,
		2,
		"job-prev",
	)
	if record.RequestID != "req-123" {
		t.Fatalf("RequestID=%q, want %q", record.RequestID, "req-123")
	}
	if record.Operation != JobOperationCreate {
		t.Fatalf("Operation=%q, want %q", record.Operation, JobOperationCreate)
	}
	if record.AttemptNo != 2 {
		t.Fatalf("AttemptNo=%d, want %d", record.AttemptNo, 2)
	}
	if record.RetryOfJobID != "job-prev" {
		t.Fatalf("RetryOfJobID=%q, want %q", record.RetryOfJobID, "job-prev")
	}
}

func TestNewRedoTemplateImageJobRecordPersistsRequestID(t *testing.T) {
	record := newRedoTemplateImageJobRecord(
		"job-redo-1",
		&types.RedoTemplateFromImageReq{
			Request:    &types.Request{RequestID: "req-redo-123"},
			TemplateID: "tpl-1",
			FailedOnly: true,
		},
		&models.TemplateImageJob{
			JobID:      "job-prev",
			ArtifactID: "artifact-1",
			Phase:      JobPhaseDistributing,
		},
		&types.CreateTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-create-1"},
			TemplateID:        "tpl-1",
			SourceImageRef:    "docker.io/library/nginx:latest",
			WritableLayerSize: "20Gi",
			InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
			NetworkType:       cubeboxv1.NetworkType_tap.String(),
		},
		`{"template_id":"tpl-1"}`,
		3,
		[]string{"node-a"},
		[]models.TemplateReplica{{NodeID: "node-a", Status: ReplicaStatusFailed}},
	)
	if record.RequestID != "req-redo-123" {
		t.Fatalf("RequestID=%q, want %q", record.RequestID, "req-redo-123")
	}
	if record.Operation != JobOperationRedo {
		t.Fatalf("Operation=%q, want %q", record.Operation, JobOperationRedo)
	}
	if record.AttemptNo != 3 {
		t.Fatalf("AttemptNo=%d, want %d", record.AttemptNo, 3)
	}
	if record.RetryOfJobID != "job-prev" {
		t.Fatalf("RetryOfJobID=%q, want %q", record.RetryOfJobID, "job-prev")
	}
	if record.RedoMode != RedoModeFailedOnly {
		t.Fatalf("RedoMode=%q, want %q", record.RedoMode, RedoModeFailedOnly)
	}
	if record.Phase == "" || record.ResumePhase == "" {
		t.Fatalf("Phase=%q ResumePhase=%q, both should be set", record.Phase, record.ResumePhase)
	}
}

func TestGenerateTemplateCreateRequestInjectsImmutableRootfsMetadata(t *testing.T) {
	req := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "template-1",
		WritableLayerSize: "20Gi",
		ExposedPorts:      []int32{80, 8080},
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
	}
	artifact := &models.RootfsArtifact{
		ArtifactID:              "artifact-1",
		TemplateSpecFingerprint: "fingerprint-1",
		Ext4SHA256:              "sha256-1",
		Ext4SizeBytes:           1024,
		DownloadToken:           "token-1",
	}
	got, err := generateTemplateCreateRequest(req, artifact, image.DockerImageConfig{
		Entrypoint: []string{"/bin/sh"},
		Cmd:        []string{"-c", "echo ok"},
		Env:        []string{"A=B"},
		WorkingDir: "/workspace",
	}, "http://master.example")
	if err != nil {
		t.Fatalf("generateTemplateCreateRequest failed: %v", err)
	}
	if got.Annotations[constants.CubeAnnotationWritableLayerSize] != "20Gi" {
		t.Fatalf("unexpected writable layer annotation: %q", got.Annotations[constants.CubeAnnotationWritableLayerSize])
	}
	if len(got.Volumes) != 1 || got.Volumes[0].VolumeSource == nil || got.Volumes[0].VolumeSource.EmptyDir == nil {
		t.Fatalf("rootfs writable volume was not injected")
	}
	if got.Volumes[0].VolumeSource.EmptyDir.SizeLimit != "20Gi" {
		t.Fatalf("unexpected size limit: %q", got.Volumes[0].VolumeSource.EmptyDir.SizeLimit)
	}
	if len(got.Containers) != 1 {
		t.Fatalf("unexpected container count: %d", len(got.Containers))
	}
	if got.Containers[0].Image == nil || got.Containers[0].Image.Image != "artifact-1" {
		t.Fatalf("artifact image was not injected")
	}
	if got.Containers[0].Image.Annotations[constants.CubeAnnotationRootfsArtifactSHA256] != "sha256-1" {
		t.Fatalf("unexpected artifact sha annotation")
	}
	if got.Annotations[constants.AnnotationsExposedPort] != "80:8080" {
		t.Fatalf("unexpected exposed ports annotation: %q", got.Annotations[constants.AnnotationsExposedPort])
	}
}

func TestGenerateTemplateCreateRequestAppliesDNSConfigOverride(t *testing.T) {
	req := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "template-1",
		WritableLayerSize: "20Gi",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
		ContainerOverrides: &types.ContainerOverrides{
			DnsConfig: &types.DNSConfig{Servers: []string{"8.8.8.8", "1.1.1.1"}},
		},
	}
	artifact := &models.RootfsArtifact{
		ArtifactID:              "artifact-1",
		TemplateSpecFingerprint: "fingerprint-1",
		Ext4SHA256:              "sha256-1",
		Ext4SizeBytes:           1024,
		DownloadToken:           "token-1",
	}
	got, err := generateTemplateCreateRequest(req, artifact, image.DockerImageConfig{}, "http://master.example")
	if err != nil {
		t.Fatalf("generateTemplateCreateRequest failed: %v", err)
	}
	if len(got.Containers) != 1 {
		t.Fatalf("unexpected container count: %d", len(got.Containers))
	}
	if got.Containers[0].DnsConfig == nil {
		t.Fatal("expected container DnsConfig to be set")
	}
	want := []string{"8.8.8.8", "1.1.1.1"}
	if !reflect.DeepEqual(got.Containers[0].DnsConfig.Servers, want) {
		t.Fatalf("DnsConfig.Servers=%v, want %v", got.Containers[0].DnsConfig.Servers, want)
	}
}

func TestGenerateTemplateCreateRequestClonesCubeNetworkRules(t *testing.T) {
	allowInternetAccess := false
	sni := "sni.example.com"
	host := "api.example.com"
	path := "/v1"
	scheme := "https"
	audit := "log-only"
	format := "bearer %s"
	req := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "template-1",
		WritableLayerSize: "20Gi",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
		CubeNetworkConfig: &types.CubeNetworkConfig{
			AllowInternetAccess: &allowInternetAccess,
			Rules: []*types.EgressRule{{
				Name: "allow-api",
				Match: &types.EgressRuleMatch{
					SNI:    &sni,
					Host:   &host,
					Method: []string{"GET"},
					Path:   &path,
					Scheme: &scheme,
				},
				Action: &types.EgressRuleAction{
					Allow: true,
					Audit: &audit,
					Inject: []*types.EgressRuleInject{{
						Header: "Authorization",
						Secret: "secret-id",
						Format: &format,
					}},
				},
			}},
		},
	}
	artifact := &models.RootfsArtifact{
		ArtifactID:              "artifact-1",
		TemplateSpecFingerprint: "fingerprint-1",
		Ext4SHA256:              "sha256-1",
		Ext4SizeBytes:           1024,
		DownloadToken:           "token-1",
	}

	got, err := generateTemplateCreateRequest(req, artifact, image.DockerImageConfig{}, "http://master.example")
	if err != nil {
		t.Fatalf("generateTemplateCreateRequest failed: %v", err)
	}
	if got.CubeNetworkConfig == nil {
		t.Fatal("expected CubeNetworkConfig to be propagated")
	}
	if len(got.CubeNetworkConfig.Rules) != 1 {
		t.Fatalf("expected 1 egress rule, got %d", len(got.CubeNetworkConfig.Rules))
	}
	if got.CubeNetworkConfig.Rules[0] == req.CubeNetworkConfig.Rules[0] {
		t.Fatal("expected egress rule to be cloned, got shared pointer")
	}
	if got.CubeNetworkConfig.Rules[0].Match == nil || got.CubeNetworkConfig.Rules[0].Match.Host == nil || *got.CubeNetworkConfig.Rules[0].Match.Host != "api.example.com" {
		t.Fatalf("unexpected cloned egress rule: %+v", got.CubeNetworkConfig.Rules[0])
	}
	if got.CubeNetworkConfig.Rules[0].Match == req.CubeNetworkConfig.Rules[0].Match {
		t.Fatal("expected egress rule match to be cloned, got shared pointer")
	}
	if got.CubeNetworkConfig.Rules[0].Match.SNI == req.CubeNetworkConfig.Rules[0].Match.SNI ||
		got.CubeNetworkConfig.Rules[0].Match.Host == req.CubeNetworkConfig.Rules[0].Match.Host ||
		got.CubeNetworkConfig.Rules[0].Match.Path == req.CubeNetworkConfig.Rules[0].Match.Path ||
		got.CubeNetworkConfig.Rules[0].Match.Scheme == req.CubeNetworkConfig.Rules[0].Match.Scheme {
		t.Fatal("expected egress rule match string pointers to be deep-cloned")
	}
	if got.CubeNetworkConfig.Rules[0].Action == nil || got.CubeNetworkConfig.Rules[0].Action == req.CubeNetworkConfig.Rules[0].Action {
		t.Fatal("expected egress rule action to be cloned")
	}
	if got.CubeNetworkConfig.Rules[0].Action.Audit == req.CubeNetworkConfig.Rules[0].Action.Audit {
		t.Fatal("expected egress rule audit pointer to be deep-cloned")
	}
	if len(got.CubeNetworkConfig.Rules[0].Action.Inject) != 1 || got.CubeNetworkConfig.Rules[0].Action.Inject[0] == req.CubeNetworkConfig.Rules[0].Action.Inject[0] {
		t.Fatal("expected egress rule inject entries to be cloned")
	}
	if got.CubeNetworkConfig.Rules[0].Action.Inject[0].Format == req.CubeNetworkConfig.Rules[0].Action.Inject[0].Format {
		t.Fatal("expected egress rule inject format pointer to be deep-cloned")
	}
}

func TestGenerateTemplateCreateRequestAddsIvshmemAnnotation(t *testing.T) {
	enabled := true
	req := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "template-1",
		WritableLayerSize: "20Gi",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
		EnableIvshmem:     &enabled,
	}
	artifact := &models.RootfsArtifact{
		ArtifactID:              "artifact-1",
		TemplateSpecFingerprint: "fingerprint-1",
		Ext4SHA256:              "sha256-1",
		Ext4SizeBytes:           1024,
		DownloadToken:           "token-1",
	}
	got, err := generateTemplateCreateRequest(req, artifact, image.DockerImageConfig{}, "http://master.example")
	if err != nil {
		t.Fatalf("generateTemplateCreateRequest failed: %v", err)
	}
	if got.Annotations[constants.CubeAnnotationEnableIvshmem] != "true" {
		t.Fatalf("expected ivshmem annotation to be set, got %q", got.Annotations[constants.CubeAnnotationEnableIvshmem])
	}
}

func TestBuildTemplateSpecFingerprintIgnoresIvshmemFlag(t *testing.T) {
	enabled := true
	reqA := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-a"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
	}
	reqB := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-b"},
		SourceImageRef:    reqA.SourceImageRef,
		WritableLayerSize: reqA.WritableLayerSize,
		InstanceType:      reqA.InstanceType,
		NetworkType:       reqA.NetworkType,
		EnableIvshmem:     &enabled,
	}
	gotA := buildTemplateSpecFingerprint(reqA, "sha256:source")
	gotB := buildTemplateSpecFingerprint(reqB, "sha256:source")
	if gotA != gotB {
		t.Fatalf("ivshmem should not affect rootfs artifact fingerprint: %q vs %q", gotA, gotB)
	}
}

func TestMarshalTemplateImageJobRequestIgnoresRequestIDAndPassword(t *testing.T) {
	reqA := &types.CreateTemplateFromImageReq{
		Request:            &types.Request{RequestID: "req-a"},
		SourceImageRef:     "docker.io/library/nginx:latest",
		RegistryPassword:   "secret-a",
		TemplateID:         "tpl-a",
		WritableLayerSize:  "1Gi",
		InstanceType:       cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:        cubeboxv1.NetworkType_tap.String(),
		ContainerOverrides: &types.ContainerOverrides{Command: []string{"echo", "ok"}},
	}
	reqB := &types.CreateTemplateFromImageReq{
		Request:            &types.Request{RequestID: "req-b"},
		SourceImageRef:     reqA.SourceImageRef,
		RegistryPassword:   "secret-b",
		TemplateID:         reqA.TemplateID,
		WritableLayerSize:  reqA.WritableLayerSize,
		InstanceType:       reqA.InstanceType,
		NetworkType:        reqA.NetworkType,
		ContainerOverrides: reqA.ContainerOverrides,
	}
	payloadA, err := marshalTemplateImageJobRequest(reqA)
	if err != nil {
		t.Fatalf("marshalTemplateImageJobRequest(reqA) failed: %v", err)
	}
	payloadB, err := marshalTemplateImageJobRequest(reqB)
	if err != nil {
		t.Fatalf("marshalTemplateImageJobRequest(reqB) failed: %v", err)
	}
	if payloadA != payloadB {
		t.Fatalf("expected stable payload across request IDs, got %q vs %q", payloadA, payloadB)
	}
	if strings.Contains(payloadA, "req-a") || strings.Contains(payloadA, "secret-a") {
		t.Fatalf("stable payload leaked request-specific data: %q", payloadA)
	}
}

func TestBuildCommitTemplateSpecFingerprintIgnoresRequestID(t *testing.T) {
	reqA := &types.CreateCubeSandboxReq{
		Request:      &types.Request{RequestID: "req-a"},
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:  cubeboxv1.NetworkType_tap.String(),
		Annotations: map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateID:      "tpl-a",
			constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
		},
	}
	reqB := &types.CreateCubeSandboxReq{
		Request:      &types.Request{RequestID: "req-b"},
		InstanceType: reqA.InstanceType,
		NetworkType:  reqA.NetworkType,
		Annotations: map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateID:      "tpl-a",
			constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
		},
	}
	payloadA, err := marshalTemplateCommitJobRequest(reqA)
	if err != nil {
		t.Fatalf("marshalTemplateCommitJobRequest(reqA) failed: %v", err)
	}
	payloadB, err := marshalTemplateCommitJobRequest(reqB)
	if err != nil {
		t.Fatalf("marshalTemplateCommitJobRequest(reqB) failed: %v", err)
	}
	if gotA, gotB := buildCommitTemplateSpecFingerprintFromSnapshot(payloadA), buildCommitTemplateSpecFingerprintFromSnapshot(payloadB); gotA != gotB {
		t.Fatalf("expected identical fingerprint, got %q vs %q", gotA, gotB)
	}
}

func TestTemplateInfoFromJobFallsBackToLatestAttemptStatus(t *testing.T) {
	createdAt := time.Date(2026, time.April, 2, 8, 10, 30, 0, time.FixedZone("UTC+8", 8*3600))
	info := templateInfoFromJob(&models.TemplateImageJob{
		TemplateID:        "tpl-a",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		Status:            JobStatusRunning,
		ErrorMessage:      "building",
		SourceImageRef:    "docker.io/library/nginx:latest",
		SourceImageDigest: "sha256:abcd",
		Model:             gorm.Model{CreatedAt: createdAt},
	})
	if info.Status != JobStatusRunning {
		t.Fatalf("unexpected status: %q", info.Status)
	}
	if info.LastError != "building" {
		t.Fatalf("unexpected last error: %q", info.LastError)
	}
	if info.CreatedAt != "2026-04-02T00:10:30Z" {
		t.Fatalf("unexpected created_at: %q", info.CreatedAt)
	}
	if info.ImageInfo != "docker.io/library/nginx:latest@sha256:abcd" {
		t.Fatalf("unexpected image_info: %q", info.ImageInfo)
	}
}

func TestComposeImageInfo(t *testing.T) {
	tests := []struct {
		name   string
		ref    string
		digest string
		want   string
	}{
		{
			name:   "ref only",
			ref:    "docker.io/library/nginx:latest",
			digest: "",
			want:   "docker.io/library/nginx:latest",
		},
		{
			name:   "ref and digest",
			ref:    "docker.io/library/nginx:latest",
			digest: "sha256:abcd",
			want:   "docker.io/library/nginx:latest@sha256:abcd",
		},
		{
			name:   "already digest ref",
			ref:    "docker.io/library/nginx@sha256:abcd",
			digest: "sha256:abcd",
			want:   "docker.io/library/nginx@sha256:abcd",
		},
		{
			name:   "digest carries canonical name prefix",
			ref:    "docker.io/library/nginx:latest",
			digest: "docker.io/library/nginx@sha256:abcd",
			want:   "docker.io/library/nginx:latest@sha256:abcd",
		},
	}
	for _, tc := range tests {
		if got := composeImageInfo(tc.ref, tc.digest); got != tc.want {
			t.Fatalf("%s: composeImageInfo(%q,%q)=%q, want %q", tc.name, tc.ref, tc.digest, got, tc.want)
		}
	}
}

func TestExtractImageInfoFromRequestJSON(t *testing.T) {
	payload := `{"containers":[{"name":"main","image":{"image":"docker.io/library/python:3.11@sha256:aaaa"}}]}`
	got := extractImageInfoFromRequestJSON(payload)
	if got != "docker.io/library/python:3.11@sha256:aaaa" {
		t.Fatalf("extractImageInfoFromRequestJSON()=%q", got)
	}
}

func TestExtractImageInfoFromRequestJSONFallbacks(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "invalid json",
			payload: `{"containers":`,
			want:    "",
		},
		{
			name:    "no containers",
			payload: `{"annotations":{"k":"v"}}`,
			want:    "",
		},
		{
			name:    "container without image",
			payload: `{"containers":[{"name":"main"}]}`,
			want:    "",
		},
		{
			name:    "container image without digest",
			payload: `{"containers":[{"image":{"image":"docker.io/library/python:3.11"}}]}`,
			want:    "docker.io/library/python:3.11",
		},
	}
	for _, tc := range tests {
		if got := extractImageInfoFromRequestJSON(tc.payload); got != tc.want {
			t.Fatalf("%s: got=%q want=%q", tc.name, got, tc.want)
		}
	}
}

func TestFormatUTCRFC3339(t *testing.T) {
	if got := formatUTCRFC3339(time.Time{}); got != "" {
		t.Fatalf("zero time should be empty, got %q", got)
	}
	ts := time.Date(2026, time.April, 2, 8, 10, 30, 0, time.FixedZone("UTC+8", 8*3600))
	if got := formatUTCRFC3339(ts); got != "2026-04-02T00:10:30Z" {
		t.Fatalf("unexpected UTC format: %q", got)
	}
}

func TestTemplateInfoFromJobIncludesLatestJobID(t *testing.T) {
	info := templateInfoFromJob(&models.TemplateImageJob{
		TemplateID: "tpl-a",
		JobID:      "job-build-1",
		Status:     JobStatusRunning,
	})
	if info.JobID != "job-build-1" {
		t.Fatalf("expected running job id, got %q", info.JobID)
	}

	done := templateInfoFromJob(&models.TemplateImageJob{
		TemplateID: "tpl-b",
		JobID:      "job-done-1",
		Status:     JobStatusReady,
	})
	if done.JobID != "job-done-1" {
		t.Fatalf("expected terminal job id, got %q", done.JobID)
	}

	failed := templateInfoFromJob(&models.TemplateImageJob{
		TemplateID: "tpl-c",
		JobID:      "job-failed-1",
		Status:     JobStatusFailed,
	})
	if failed.JobID != "job-failed-1" {
		t.Fatalf("expected failed job id, got %q", failed.JobID)
	}
}

func TestTemplateInfoFromJobPrefersTemplateStatus(t *testing.T) {
	info := templateInfoFromJob(&models.TemplateImageJob{
		TemplateID:     "tpl-a",
		Status:         JobStatusRunning,
		TemplateStatus: StatusReady,
	})
	if info.Status != StatusReady {
		t.Fatalf("expected template_status to override job status, got %q", info.Status)
	}
}

func TestValidateReusableRootfsArtifactAllowsLegacyFingerprintlessRecord(t *testing.T) {
	record, err := validateReusableRootfsArtifact(&models.RootfsArtifact{
		ArtifactID: "rfs-legacy",
	}, "fingerprint-1", "rfs-legacy")
	if err != nil {
		t.Fatalf("validateReusableRootfsArtifact failed: %v", err)
	}
	if record == nil || record.ArtifactID != "rfs-legacy" {
		t.Fatalf("unexpected record: %#v", record)
	}
}

func TestValidateReusableRootfsArtifactRejectsFingerprintMismatch(t *testing.T) {
	_, err := validateReusableRootfsArtifact(&models.RootfsArtifact{
		ArtifactID:              "rfs-legacy",
		TemplateSpecFingerprint: "fingerprint-old",
	}, "fingerprint-new", "rfs-legacy")
	if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateReusableRootfsArtifactRejectsArtifactIDMismatch(t *testing.T) {
	_, err := validateReusableRootfsArtifact(&models.RootfsArtifact{
		ArtifactID: "rfs-other",
	}, "fingerprint-1", "rfs-expected")
	if err == nil || !strings.Contains(err.Error(), "artifact id mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateReusableRootfsArtifactHandlesMissingRecord(t *testing.T) {
	_, err := validateReusableRootfsArtifact(nil, "fingerprint-1", "rfs-expected")
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRootfsArtifactSoftDeleted(t *testing.T) {
	if rootfsArtifactSoftDeleted(nil) {
		t.Fatal("nil record should not be treated as deleted")
	}
	if rootfsArtifactSoftDeleted(&models.RootfsArtifact{}) {
		t.Fatal("zero-value record should not be treated as deleted")
	}
	record := &models.RootfsArtifact{}
	record.DeletedAt.Valid = true
	if !rootfsArtifactSoftDeleted(record) {
		t.Fatal("soft-deleted record should be detected")
	}
}

func TestManagedArtifactDirRecognizesWorkAndStoreRoots(t *testing.T) {
	workRoot := filepath.Join(t.TempDir(), "work")
	storeRoot := filepath.Join(t.TempDir(), "store")
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_DIR", workRoot)
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", storeRoot)

	if dir, ok := managedArtifactDir("artifact-1", filepath.Join(workRoot, "artifact-1", "artifact-1.ext4")); !ok || dir != filepath.Join(workRoot, "artifact-1") {
		t.Fatalf("managedArtifactDir should accept work root, got dir=%q ok=%v", dir, ok)
	}
	if dir, ok := managedArtifactDir("artifact-2", filepath.Join(storeRoot, "artifact-2", "artifact-2.ext4")); !ok || dir != filepath.Join(storeRoot, "artifact-2") {
		t.Fatalf("managedArtifactDir should accept store root, got dir=%q ok=%v", dir, ok)
	}
	if _, ok := managedArtifactDir("artifact-3", filepath.Join(t.TempDir(), "artifact-3", "artifact-3.ext4")); ok {
		t.Fatal("managedArtifactDir should reject unmanaged roots")
	}
}

func TestManagedArtifactDirRecognizesFallbackStoreRoot(t *testing.T) {
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", "")
	fallbackRoot := image.ArtifactFallbackStoreRootDir()
	if dir, ok := managedArtifactDir("artifact-fallback", filepath.Join(fallbackRoot, "artifact-fallback", "artifact-fallback.ext4")); !ok || dir != filepath.Join(fallbackRoot, "artifact-fallback") {
		t.Fatalf("managedArtifactDir should accept fallback store root, got dir=%q ok=%v", dir, ok)
	}
}

func TestCleanupLocalRootfsArtifactRemovesManagedDirectory(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", storeRoot)

	artifactDir := filepath.Join(storeRoot, "artifact-1")
	if err := os.MkdirAll(filepath.Join(artifactDir, "rootfs"), 0o755); err != nil {
		t.Fatalf("MkdirAll artifactDir failed: %v", err)
	}
	ext4Path := filepath.Join(artifactDir, "artifact-1.ext4")
	if err := os.WriteFile(ext4Path, []byte("ext4"), 0o644); err != nil {
		t.Fatalf("WriteFile ext4Path failed: %v", err)
	}

	if err := cleanupLocalRootfsArtifact("artifact-1", ext4Path); err != nil {
		t.Fatalf("cleanupLocalRootfsArtifact failed: %v", err)
	}
	if _, err := os.Stat(artifactDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifactDir should be removed, err=%v", err)
	}
}

func TestCleanupFailedRootfsArtifactDelegatesToLastOwnerCleanup(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	var (
		gotArtifactID   string
		gotInstanceType string
		gotExclude      string
	)
	patches.ApplyFunc(cleanupArtifactFully, func(ctx context.Context, artifactID, instanceType, excludeTemplateID string) error {
		gotArtifactID = artifactID
		gotInstanceType = instanceType
		gotExclude = excludeTemplateID
		return nil
	})

	if err := cleanupFailedRootfsArtifact(context.Background(), &models.RootfsArtifact{
		ArtifactID: "artifact-1",
		Ext4Path:   filepath.Join(t.TempDir(), "artifact-1", "artifact-1.ext4"),
	}, cubeboxv1.InstanceType_cubebox.String(), "tpl-owner"); err != nil {
		t.Fatalf("cleanupFailedRootfsArtifact returned error: %v", err)
	}
	if gotArtifactID != "artifact-1" {
		t.Fatalf("artifactID not forwarded, got %q", gotArtifactID)
	}
	if gotInstanceType != cubeboxv1.InstanceType_cubebox.String() {
		t.Fatalf("instanceType not forwarded, got %q", gotInstanceType)
	}
	// The build's own template must be excluded so its in-flight job/definition
	// does not pin the artifact and block its own failure cleanup.
	if gotExclude != "tpl-owner" {
		t.Fatalf("expected own template excluded, got %q", gotExclude)
	}
}

func TestBuildRootfsArtifactFinalizesBuildResult(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	artifact := &models.RootfsArtifact{
		ArtifactID:              "artifact-1",
		TemplateSpecFingerprint: "fingerprint-1",
		WritableLayerSize:       "20Gi",
	}
	source := &image.PreparedSource{
		Digest:       "sha256:digest",
		MasterNodeIP: "http://master.example",
		ConfigJSON:   `{"Cmd":["nginx"]}`,
		Config:       image.DockerImageConfig{Cmd: []string{"nginx"}, Env: []string{"A=B"}},
	}
	req := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		TemplateID:        "tpl-1",
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
	}

	patches.ApplyFunc(image.BuildExt4, func(ctx context.Context, src *image.PreparedSource, opts image.BuildOptions) (image.BuildResult, error) {
		if src != source {
			t.Fatalf("unexpected source passed to BuildExt4: %#v", src)
		}
		if opts.ArtifactID != artifact.ArtifactID {
			t.Fatalf("BuildOptions.ArtifactID=%q, want %q", opts.ArtifactID, artifact.ArtifactID)
		}
		return image.BuildResult{Ext4Path: "/tmp/artifact-1.ext4", SHA256: "sha-ext4", SizeBytes: 1234}, nil
	})

	var updateValues map[string]any
	patches.ApplyFunc(updateRootfsArtifact, func(ctx context.Context, artifactID string, values map[string]any) error {
		if artifactID != artifact.ArtifactID {
			t.Fatalf("artifactID=%q, want %q", artifactID, artifact.ArtifactID)
		}
		updateValues = values
		return nil
	})
	patches.ApplyFunc(getRootfsArtifactByID, func(ctx context.Context, artifactID string) (*models.RootfsArtifact, error) {
		if artifactID != artifact.ArtifactID {
			t.Fatalf("artifactID=%q, want %q", artifactID, artifact.ArtifactID)
		}
		latest := *artifact
		return &latest, nil
	})

	got, generatedReq, err := buildRootfsArtifact(context.Background(), artifact, req, source, "http://master.example", nil, "")
	if err != nil {
		t.Fatalf("buildRootfsArtifact failed: %v", err)
	}
	if got.Ext4Path != "/tmp/artifact-1.ext4" || got.Ext4SHA256 != "sha-ext4" || got.Ext4SizeBytes != 1234 {
		t.Fatalf("artifact build result was not finalized: %#v", got)
	}
	if got.Status != ArtifactStatusReady {
		t.Fatalf("Status=%q, want READY", got.Status)
	}
	if got.DownloadToken == "" {
		t.Fatal("DownloadToken should be generated")
	}
	if generatedReq == nil || len(generatedReq.Containers) != 1 || generatedReq.Containers[0].Image == nil {
		t.Fatalf("generated request missing rootfs image: %#v", generatedReq)
	}
	if generatedReq.Containers[0].Image.Annotations[constants.CubeAnnotationRootfsArtifactSHA256] != "sha-ext4" {
		t.Fatalf("generated request did not include ext4 sha annotation")
	}
	if updateValues["ext4_path"] != "/tmp/artifact-1.ext4" ||
		updateValues["ext4_sha256"] != "sha-ext4" ||
		updateValues["ext4_size_bytes"] != int64(1234) ||
		updateValues["status"] != ArtifactStatusReady {
		t.Fatalf("unexpected persisted values: %#v", updateValues)
	}
	if _, ok := updateValues["generated_request_json"].(string); !ok {
		t.Fatalf("generated_request_json was not persisted as string: %#v", updateValues)
	}
}

func TestNormalizeRedoTemplateImageRequest(t *testing.T) {
	got, err := normalizeRedoTemplateImageRequest(&types.RedoTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		TemplateID:        "tpl-1",
		DistributionScope: []string{"node-a"},
		FailedOnly:        true,
	})
	if err != nil {
		t.Fatalf("normalizeRedoTemplateImageRequest failed: %v", err)
	}
	if got.TemplateID != "tpl-1" {
		t.Fatalf("unexpected template id: %q", got.TemplateID)
	}
	if !reflect.DeepEqual(got.DistributionScope, []string{"node-a"}) {
		t.Fatalf("unexpected distribution scope: %v", got.DistributionScope)
	}
	if !got.FailedOnly {
		t.Fatal("expected failed_only to be preserved")
	}
}

func TestDetermineRedoModeSupportsScopedFailures(t *testing.T) {
	if got := determineRedoMode(&types.RedoTemplateFromImageReq{
		TemplateID:        "tpl-1",
		DistributionScope: []string{"node-a"},
		FailedOnly:        true,
	}); got != RedoModeFailedNodes {
		t.Fatalf("determineRedoMode()=%q, want %q", got, RedoModeFailedNodes)
	}
}

func TestResolveRedoTargetsIntersectsFailedOnlyWithScope(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(healthyTemplateNodes, func(instanceType string) []*node.Node {
		return []*node.Node{
			{InsID: "node-a", IP: "10.0.0.1", Healthy: true},
			{InsID: "node-b", IP: "10.0.0.2", Healthy: true},
		}
	})
	targets, err := resolveRedoTargets(cubeboxv1.InstanceType_cubebox.String(), &types.RedoTemplateFromImageReq{
		TemplateID:        "tpl-1",
		DistributionScope: []string{"node-a", "node-b"},
		FailedOnly:        true,
	}, []models.TemplateReplica{
		{NodeID: "node-a", Status: ReplicaStatusFailed},
		{NodeID: "node-b", Status: ReplicaStatusReady},
	})
	if err != nil {
		t.Fatalf("resolveRedoTargets failed: %v", err)
	}
	if len(targets) != 1 || targets[0].ID() != "node-a" {
		t.Fatalf("unexpected targets: %#v", targets)
	}
}

func TestResolveRedoTargetsRejectsWhenNoFailedReplicas(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(healthyTemplateNodes, func(instanceType string) []*node.Node {
		return []*node.Node{{InsID: "node-a", IP: "10.0.0.1", Healthy: true}}
	})
	_, err := resolveRedoTargets(cubeboxv1.InstanceType_cubebox.String(), &types.RedoTemplateFromImageReq{
		TemplateID: "tpl-1",
		FailedOnly: true,
	}, []models.TemplateReplica{
		{NodeID: "node-a", Status: ReplicaStatusReady},
	})
	if !errors.Is(err, ErrNoFailedTemplateReplicas) {
		t.Fatalf("expected ErrNoFailedTemplateReplicas, got %v", err)
	}
}

func TestDetermineRedoResumePhase(t *testing.T) {
	tests := []struct {
		name    string
		job     *models.TemplateImageJob
		replica []models.TemplateReplica
		want    string
	}{
		{
			name: "build failure resumes distribution build pipeline",
			job:  &models.TemplateImageJob{Phase: JobPhaseBuildingExt4},
			want: JobPhaseBuildingExt4,
		},
		{
			name: "distribution failure resumes distribution",
			replica: []models.TemplateReplica{
				{Status: ReplicaStatusFailed, LastErrorPhase: ReplicaPhaseDistributing},
			},
			want: JobPhaseDistributing,
		},
		{
			name: "snapshot failure resumes snapshotting",
			job:  &models.TemplateImageJob{Phase: JobPhaseCreatingTemplate},
			want: JobPhaseSnapshotting,
		},
	}
	for _, tc := range tests {
		if got := determineRedoResumePhase(tc.job, tc.replica); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestJobModelToInfoIncludesRedoMetadata(t *testing.T) {
	info, err := jobModelToInfo(context.Background(), &models.TemplateImageJob{
		JobID:         "job-1",
		TemplateID:    "tpl-1",
		Operation:     JobOperationRedo,
		RedoMode:      RedoModeFailedOnly,
		RedoScopeJSON: `["node-a","10.0.0.2"]`,
		ResumePhase:   JobPhaseSnapshotting,
		Status:        JobStatusRunning,
		Phase:         JobPhaseSnapshotting,
	})
	if err != nil {
		t.Fatalf("jobModelToInfo failed: %v", err)
	}
	if info.Operation != JobOperationRedo {
		t.Fatalf("unexpected operation: %q", info.Operation)
	}
	if info.RedoMode != RedoModeFailedOnly {
		t.Fatalf("unexpected redo mode: %q", info.RedoMode)
	}
	if info.ResumePhase != JobPhaseSnapshotting {
		t.Fatalf("unexpected resume phase: %q", info.ResumePhase)
	}
	if !reflect.DeepEqual(info.RedoScope, []string{"node-a", "10.0.0.2"}) {
		t.Fatalf("unexpected redo scope: %v", info.RedoScope)
	}
}

func TestRunRedoTemplateImageJobStopsOnArtifactCleanupFailure(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	targets := []*node.Node{{InsID: "node-a", IP: "10.0.0.1", Healthy: true}}
	generatedReqPayload, _ := json.Marshal(&types.CreateCubeSandboxReq{
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
		Annotations: map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateID:      "tpl-1",
			constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
		},
	})

	var lastUpdate map[string]any
	patches.ApplyFunc(getTemplateImageJobRecordByID, func(ctx context.Context, jobID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{
			JobID:       jobID,
			TemplateID:  "tpl-1",
			ResumePhase: JobPhaseDistributing,
			ArtifactID:  "artifact-1",
		}, nil
	})
	patches.ApplyFunc(updateTemplateImageJob, func(ctx context.Context, jobID string, values map[string]any) error {
		lastUpdate = values
		return nil
	})
	patches.ApplyFunc(unmarshalTemplateImageJobRequest, func(payload string) (*types.CreateTemplateFromImageReq, error) {
		return &types.CreateTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-1"},
			TemplateID:        "tpl-1",
			InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
			WritableLayerSize: "20Gi",
			SourceImageRef:    "docker.io/library/nginx:latest",
		}, nil
	})
	patches.ApplyFunc(ListReplicas, func(ctx context.Context, templateID string) ([]models.TemplateReplica, error) {
		return []models.TemplateReplica{{NodeID: "node-a", Status: ReplicaStatusFailed}}, nil
	})
	patches.ApplyFunc(resolveRedoTargets, func(instanceType string, req *types.RedoTemplateFromImageReq, replicas []models.TemplateReplica) ([]*node.Node, error) {
		return targets, nil
	})
	patches.ApplyFunc(getRootfsArtifactByID, func(ctx context.Context, artifactID string) (*models.RootfsArtifact, error) {
		return &models.RootfsArtifact{
			ArtifactID:           artifactID,
			GeneratedRequestJSON: string(generatedReqPayload),
		}, nil
	})
	patches.ApplyFunc(cleanupArtifactOnNodes, func(ctx context.Context, artifactID, instanceType string, targets []*node.Node) error {
		return errors.New("cleanup image failed")
	})
	patches.ApplyFunc(distributeRootfsArtifact, func(ctx context.Context, req *types.CreateTemplateFromImageReq, generatedReq *types.CreateCubeSandboxReq, artifact *models.RootfsArtifact, templateID, jobID string) ([]*node.Node, int32, int32, int32, error) {
		t.Fatal("distributeRootfsArtifact should not be called after cleanup failure")
		return nil, 0, 0, 0, nil
	})

	runRedoTemplateImageJob(context.Background(), "job-1", &types.RedoTemplateFromImageReq{
		Request:    &types.Request{RequestID: "req-redo"},
		TemplateID: "tpl-1",
	}, "http://master.example")

	if lastUpdate == nil {
		t.Fatal("expected job status update")
	}
	if lastUpdate["status"] != JobStatusFailed {
		t.Fatalf("unexpected status update: %+v", lastUpdate)
	}
	if got, _ := lastUpdate["error_message"].(string); !strings.Contains(got, "cleanup artifact before redistribute failed") {
		t.Fatalf("unexpected error message: %q", got)
	}
}

func TestRunRedoTemplateImageJobRegeneratesRequestForRedoTemplateID(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	const redoTemplateID = "tpl-redo"
	const staleTemplateID = "tpl-stale"
	targets := []*node.Node{{InsID: "node-a", IP: "10.0.0.1", Healthy: true}}
	staleReqPayload, _ := json.Marshal(&types.CreateCubeSandboxReq{
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
		Annotations: map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateID:      staleTemplateID,
			constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
		},
	})
	imageConfigPayload, _ := json.Marshal(image.DockerImageConfig{
		Entrypoint: []string{"/bin/sh"},
		Cmd:        []string{"-c", "echo ok"},
	})

	patches.ApplyFunc(getTemplateImageJobRecordByID, func(ctx context.Context, jobID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{
			JobID:       jobID,
			TemplateID:  redoTemplateID,
			ResumePhase: JobPhaseSnapshotting,
			ArtifactID:  "artifact-1",
		}, nil
	})
	patches.ApplyFunc(updateTemplateImageJob, func(ctx context.Context, jobID string, values map[string]any) error {
		return nil
	})
	patches.ApplyFunc(unmarshalTemplateImageJobRequest, func(payload string) (*types.CreateTemplateFromImageReq, error) {
		return &types.CreateTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-1"},
			TemplateID:        staleTemplateID,
			InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
			NetworkType:       cubeboxv1.NetworkType_tap.String(),
			WritableLayerSize: "20Gi",
			SourceImageRef:    "docker.io/library/nginx:latest",
		}, nil
	})
	patches.ApplyFunc(ListReplicas, func(ctx context.Context, templateID string) ([]models.TemplateReplica, error) {
		return []models.TemplateReplica{{NodeID: "node-a", Status: ReplicaStatusFailed}}, nil
	})
	patches.ApplyFunc(resolveRedoTargets, func(instanceType string, req *types.RedoTemplateFromImageReq, replicas []models.TemplateReplica) ([]*node.Node, error) {
		return targets, nil
	})
	patches.ApplyFunc(getRootfsArtifactByID, func(ctx context.Context, artifactID string) (*models.RootfsArtifact, error) {
		return &models.RootfsArtifact{
			ArtifactID:              artifactID,
			TemplateSpecFingerprint: "fingerprint-1",
			Ext4SHA256:              "sha256",
			Ext4SizeBytes:           1024,
			DownloadToken:           "token-1",
			GeneratedRequestJSON:    string(staleReqPayload),
			ImageConfigJSON:         string(imageConfigPayload),
			SourceImageDigest:       "sha256:digest",
			WritableLayerSize:       "20Gi",
			Status:                  ArtifactStatusReady,
		}, nil
	})
	patches.ApplyFunc(cleanupTemplateReplicasOnNodes, func(ctx context.Context, templateID string, replicas []models.TemplateReplica, targets []*node.Node) error {
		if templateID != redoTemplateID {
			t.Fatalf("cleanup templateID = %q, want %q", templateID, redoTemplateID)
		}
		return nil
	})
	patches.ApplyFunc(ensureTemplateDefinitionWithOptions, func(ctx context.Context, templateID string, storedReq *types.CreateCubeSandboxReq, instanceType, version string, _ definitionCreateOptions) (bool, error) {
		if templateID != redoTemplateID {
			t.Fatalf("definition templateID = %q, want %q", templateID, redoTemplateID)
		}
		if got := storedReq.Annotations[constants.CubeAnnotationAppSnapshotTemplateID]; got != redoTemplateID {
			t.Fatalf("stored request templateID = %q, want %q", got, redoTemplateID)
		}
		return true, nil
	})

	var capturedReq *types.CreateCubeSandboxReq
	patches.ApplyFunc(createTemplateReplicasOnNodes, func(ctx context.Context, templateID string, req *types.CreateCubeSandboxReq, targets []*node.Node, opts replicaRunOptions) ([]ReplicaStatus, error) {
		if templateID != redoTemplateID {
			t.Fatalf("replica templateID = %q, want %q", templateID, redoTemplateID)
		}
		capturedReq = req
		return []ReplicaStatus{{NodeID: "node-a", Status: ReplicaStatusReady}}, nil
	})
	patches.ApplyFunc(refreshTemplateReplicaSummary, func(ctx context.Context, templateID string) error {
		return nil
	})
	patches.ApplyFunc(GetTemplateInfo, func(ctx context.Context, templateID string) (*TemplateInfo, error) {
		return &TemplateInfo{TemplateID: templateID, Status: StatusReady}, nil
	})

	runRedoTemplateImageJob(context.Background(), "job-1", &types.RedoTemplateFromImageReq{
		Request:    &types.Request{RequestID: "req-redo"},
		TemplateID: redoTemplateID,
	}, "http://master.example")

	if capturedReq == nil {
		t.Fatal("expected generated request to be passed to replica creation")
	}
	if got := capturedReq.Annotations[constants.CubeAnnotationAppSnapshotTemplateID]; got != redoTemplateID {
		t.Fatalf("generated request templateID = %q, want %q", got, redoTemplateID)
	}
	if got := capturedReq.Containers[0].Image.Annotations[constants.CubeAnnotationRootfsArtifactURL]; !strings.Contains(got, "artifact_id=artifact-1") {
		t.Fatalf("generated request should rebuild artifact URL, got %q", got)
	}
	if got := capturedReq.Containers[0].Command; !reflect.DeepEqual(got, []string{"/bin/sh"}) {
		t.Fatalf("generated request command = %v, want image config entrypoint", got)
	}
}

func TestRunRedoTemplateImageJobRequiresLocalImageForBuildRedo(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	targets := []*node.Node{{InsID: "node-a", IP: "10.0.0.1", Healthy: true}}
	var lastUpdate map[string]any
	patches.ApplyFunc(getTemplateImageJobRecordByID, func(ctx context.Context, jobID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{
			JobID:       jobID,
			TemplateID:  "tpl-1",
			ResumePhase: JobPhaseBuildingExt4,
		}, nil
	})
	patches.ApplyFunc(updateTemplateImageJob, func(ctx context.Context, jobID string, values map[string]any) error {
		lastUpdate = values
		return nil
	})
	patches.ApplyFunc(unmarshalTemplateImageJobRequest, func(payload string) (*types.CreateTemplateFromImageReq, error) {
		return &types.CreateTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-1"},
			TemplateID:        "tpl-1",
			InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
			WritableLayerSize: "20Gi",
			SourceImageRef:    "private.example/app:latest",
		}, nil
	})
	patches.ApplyFunc(ListReplicas, func(ctx context.Context, templateID string) ([]models.TemplateReplica, error) {
		return []models.TemplateReplica{{NodeID: "node-a", Status: ReplicaStatusFailed}}, nil
	})
	patches.ApplyFunc(resolveRedoTargets, func(instanceType string, req *types.RedoTemplateFromImageReq, replicas []models.TemplateReplica) ([]*node.Node, error) {
		return targets, nil
	})
	patches.ApplyFunc(image.PrepareLocalSource, func(ctx context.Context, spec image.SourceSpec) (*image.PreparedSource, error) {
		return nil, errors.New("redo requires source image private.example/app:latest to still exist locally")
	})

	runRedoTemplateImageJob(context.Background(), "job-2", &types.RedoTemplateFromImageReq{
		Request:    &types.Request{RequestID: "req-redo"},
		TemplateID: "tpl-1",
	}, "http://master.example")

	if lastUpdate == nil {
		t.Fatal("expected job status update")
	}
	if lastUpdate["status"] != JobStatusFailed {
		t.Fatalf("unexpected status update: %+v", lastUpdate)
	}
	if got, _ := lastUpdate["error_message"].(string); !strings.Contains(got, "still exist locally") {
		t.Fatalf("unexpected error message: %q", got)
	}
}

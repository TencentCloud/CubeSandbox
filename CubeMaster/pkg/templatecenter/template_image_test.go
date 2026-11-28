// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/hostcompat"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/qos"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
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
	if req.Backend != "" {
		t.Fatalf("Backend=%q, omit must stay empty for historical create-from-image", req.Backend)
	}
}

func TestNormalizeTemplateImageRequestBackendS3(t *testing.T) {
	req, err := normalizeTemplateImageRequest(&types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
		Backend:           "S3",
	})
	if err != nil {
		t.Fatalf("normalizeTemplateImageRequest failed: %v", err)
	}
	if req.Backend != "s3" {
		t.Fatalf("Backend=%q, want s3", req.Backend)
	}
}

func TestNormalizeTemplateImageRequestRejectsUnknownBackend(t *testing.T) {
	_, err := normalizeTemplateImageRequest(&types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
		Backend:           "nfs",
	})
	if err == nil {
		t.Fatal("expected unsupported backend error")
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

func TestNormalizeTemplateImageRequestAllowsMoreThanThreeCustomExposedPorts(t *testing.T) {
	req, err := normalizeTemplateImageRequest(&types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		WritableLayerSize: "20Gi",
		ExposedPorts:      []int32{9000, 9001, 9002, 9003, 80},
	})
	if err != nil {
		t.Fatalf("normalizeTemplateImageRequest failed: %v", err)
	}
	want := []int32{80, 9000, 9001, 9002, 9003}
	if !reflect.DeepEqual(req.ExposedPorts, want) {
		t.Fatalf("ExposedPorts=%v, want %v", req.ExposedPorts, want)
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

func TestNormalizeRequestKeepsProbeInitialDelay(t *testing.T) {
	// NormalizeRequest feeds the build request in CreateTemplate, and the
	// build sandbox boots the image for the first time — it may legitimately
	// need the caller's initial delay. Only the persisted definition is
	// stripped (see TestNormalizeStoredRequestStripsProbeInitialDelay).
	req, _, err := NormalizeRequest(&types.CreateCubeSandboxReq{
		Request: &types.Request{RequestID: "req-1"},
		Annotations: map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
		},
		Containers: []*types.Container{
			{
				Probe: &types.Probe{
					InitialDelayMs:      5000,
					InitialDelaySeconds: 5,
					TimeoutMs:           30000,
					PeriodMs:            100,
					SuccessThreshold:    1,
					FailureThreshold:    300,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeRequest failed: %v", err)
	}
	if len(req.Containers) != 1 || req.Containers[0] == nil || req.Containers[0].Probe == nil {
		t.Fatalf("expected one container with a probe, got %+v", req.Containers)
	}
	probe := req.Containers[0].Probe
	if probe.InitialDelayMs != 5000 || probe.InitialDelaySeconds != 5 {
		t.Fatalf("build request probe must keep the initial delay, got %+v", probe)
	}
}

func TestNormalizeStoredRequestStripsProbeInitialDelay(t *testing.T) {
	// The stored definition is what future restores replay. Sandboxes
	// restored from the snapshot resume with services already running, so
	// the persisted probe must not inherit the build-only initial delay.
	out, err := normalizeStoredTemplateRequest(&types.CreateCubeSandboxReq{
		Request: &types.Request{RequestID: "req-1"},
		Annotations: map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
		},
		Containers: []*types.Container{
			{
				Probe: &types.Probe{
					InitialDelayMs:      5000,
					InitialDelaySeconds: 5,
					TimeoutMs:           30000,
					PeriodMs:            100,
					SuccessThreshold:    1,
					FailureThreshold:    300,
				},
			},
		},
	}, true)
	if err != nil {
		t.Fatalf("normalizeStoredTemplateRequest failed: %v", err)
	}
	if len(out.Containers) != 1 || out.Containers[0] == nil || out.Containers[0].Probe == nil {
		t.Fatalf("expected one container with a probe, got %+v", out.Containers)
	}
	probe := out.Containers[0].Probe
	if probe.InitialDelayMs != 0 || probe.InitialDelaySeconds != 0 {
		t.Fatalf("stored probe must not inherit the build-only initial delay, got %+v", probe)
	}
	if probe.TimeoutMs != 30000 || probe.PeriodMs != 100 || probe.FailureThreshold != 300 || probe.SuccessThreshold != 1 {
		t.Fatalf("other probe fields must be preserved, got %+v", probe)
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
	got, err := generateTemplateCreateRequest(context.Background(), req, artifact, DockerImageConfig{
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
	if got.Containers[0].Image.Annotations[constants.CubeAnnotationRootfsArtifactURL] != "http://master.example/cube/template/artifact/download?artifact_id=artifact-1&token=token-1" {
		t.Fatalf("unexpected artifact download url: %q", got.Containers[0].Image.Annotations[constants.CubeAnnotationRootfsArtifactURL])
	}
	if got.Containers[0].Image.Annotations[constants.CubeAnnotationRootfsArtifactSHA256] != "sha256-1" {
		t.Fatalf("unexpected artifact sha annotation")
	}
	if got.Annotations[constants.AnnotationsExposedPort] != "80:8080" {
		t.Fatalf("unexpected exposed ports annotation: %q", got.Annotations[constants.AnnotationsExposedPort])
	}
	if got.Backend != "" {
		t.Fatalf("Backend=%q, omit must not invent a backend", got.Backend)
	}
	if _, ok := got.Annotations[constants.CubeAnnotationStorageBackend]; ok {
		t.Fatal("omitted backend must not inject cube.master.storage.backend")
	}
	if _, ok := got.Annotations[constants.CubeAnnotationsNetWork]; ok {
		t.Fatalf("opt-out template unexpectedly has network qos annotation: %q", got.Annotations[constants.CubeAnnotationsNetWork])
	}
	if _, ok := got.Annotations[constants.CubeAnnotationsBlkQos]; ok {
		t.Fatalf("opt-out template unexpectedly has block io qos annotation: %q", got.Annotations[constants.CubeAnnotationsBlkQos])
	}
}

func TestGenerateTemplateCreateRequestMergesEnvOverridesWithImageEnv(t *testing.T) {
	newReq := func(envs []*types.KeyValue) *types.CreateTemplateFromImageReq {
		return &types.CreateTemplateFromImageReq{
			Request:            &types.Request{RequestID: "req-env"},
			SourceImageRef:     "docker.io/library/nginx:latest",
			TemplateID:         "template-env",
			WritableLayerSize:  "20Gi",
			InstanceType:       cubeboxv1.InstanceType_cubebox.String(),
			NetworkType:        cubeboxv1.NetworkType_tap.String(),
			ContainerOverrides: &types.ContainerOverrides{Envs: envs},
		}
	}
	artifact := &models.RootfsArtifact{ArtifactID: "artifact-env", DownloadToken: "token-env"}
	imageCfg := DockerImageConfig{Env: []string{"PATH=/usr/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}}

	cases := []struct {
		name      string
		overrides []*types.KeyValue
		want      []*types.KeyValue
	}{
		{
			name:      "override adds a key and keeps image env",
			overrides: []*types.KeyValue{{Key: "API_KEY", Value: "secret"}},
			want: []*types.KeyValue{
				{Key: "API_KEY", Value: "secret"},
				{Key: "LANG", Value: "C.UTF-8"},
				{Key: "LC_ALL", Value: "C.UTF-8"},
				{Key: "PATH", Value: "/usr/bin"},
			},
		},
		{
			name:      "override wins for the same key",
			overrides: []*types.KeyValue{{Key: "LANG", Value: "en_US.UTF-8"}},
			want: []*types.KeyValue{
				{Key: "LANG", Value: "en_US.UTF-8"},
				{Key: "LC_ALL", Value: "C.UTF-8"},
				{Key: "PATH", Value: "/usr/bin"},
			},
		},
		{
			name:      "empty overrides keep image env",
			overrides: []*types.KeyValue{},
			want: []*types.KeyValue{
				{Key: "LANG", Value: "C.UTF-8"},
				{Key: "LC_ALL", Value: "C.UTF-8"},
				{Key: "PATH", Value: "/usr/bin"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := generateTemplateCreateRequest(context.Background(), newReq(tc.overrides), artifact, imageCfg, "http://master.example")
			if err != nil {
				t.Fatalf("generateTemplateCreateRequest failed: %v", err)
			}
			if !reflect.DeepEqual(got.Containers[0].Envs, tc.want) {
				t.Fatalf("Envs=%v, want %v", got.Containers[0].Envs, tc.want)
			}
		})
	}
}

func TestGenerateTemplateCreateRequestAppliesUser(t *testing.T) {
	req := &types.CreateTemplateFromImageReq{
		Request:            &types.Request{RequestID: "req-user"},
		SourceImageRef:     "docker.io/library/nginx:latest",
		TemplateID:         "template-user",
		WritableLayerSize:  "20Gi",
		InstanceType:       cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:        cubeboxv1.NetworkType_tap.String(),
		ContainerOverrides: &types.ContainerOverrides{},
	}
	parsed, err := ParseTemplateUser("1000:1000")
	if err != nil {
		t.Fatalf("ParseTemplateUser: %v", err)
	}
	req.ContainerOverrides.SecurityContext = parsed
	got, err := generateTemplateCreateRequest(context.Background(), req, &models.RootfsArtifact{
		ArtifactID: "artifact-user",
	}, DockerImageConfig{User: "nobody"}, "http://master.example")
	if err != nil {
		t.Fatalf("generateTemplateCreateRequest: %v", err)
	}
	sc := got.Containers[0].SecurityContext
	if sc == nil || !sc.Privileged || sc.ReadonlyRootfs {
		t.Fatalf("security context = %+v", sc)
	}
	if sc.RunAsUser == nil || sc.RunAsUser.Value != 1000 || sc.RunAsGroup == nil || sc.RunAsGroup.Value != 1000 {
		t.Fatalf("explicit user did not win: %+v", sc)
	}
	if sc.RunAsUsername != "" {
		t.Fatalf("username=%q", sc.RunAsUsername)
	}
}

func TestGenerateTemplateCreateRequestKeepsUserBuildPrivileged(t *testing.T) {
	for _, sc := range []*types.ContainerSecurityContext{
		{RunAsUser: &types.Int64Value{Value: 1000}},
		{RunAsUsername: "sandbox"},
	} {
		req := &types.CreateTemplateFromImageReq{
			Request:            &types.Request{RequestID: "req-user-priv"},
			SourceImageRef:     "docker.io/library/nginx:latest",
			TemplateID:         "template-user-priv",
			WritableLayerSize:  "20Gi",
			InstanceType:       cubeboxv1.InstanceType_cubebox.String(),
			NetworkType:        cubeboxv1.NetworkType_tap.String(),
			ContainerOverrides: &types.ContainerOverrides{SecurityContext: sc},
		}
		got, err := generateTemplateCreateRequest(context.Background(), req, &models.RootfsArtifact{
			ArtifactID: "artifact-user-priv",
		}, DockerImageConfig{}, "http://master.example")
		if err != nil {
			t.Fatalf("generateTemplateCreateRequest: %v", err)
		}
		if !got.Containers[0].SecurityContext.Privileged {
			t.Fatalf("a build with a custom user must stay privileged: %+v", got.Containers[0].SecurityContext)
		}
	}
}

func TestGenerateTemplateCreateRequestIgnoresImageUserWhenUnset(t *testing.T) {
	req := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-image-user"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "template-image-user",
		WritableLayerSize: "20Gi",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
	}
	got, err := generateTemplateCreateRequest(context.Background(), req, &models.RootfsArtifact{
		ArtifactID: "artifact-image-user",
	}, DockerImageConfig{User: "sandbox:1000"}, "http://master.example")
	if err != nil {
		t.Fatalf("generateTemplateCreateRequest: %v", err)
	}
	sc := got.Containers[0].SecurityContext
	if sc == nil || !sc.Privileged || sc.ReadonlyRootfs || sc.RunAsUsername != "" || sc.RunAsUser != nil || sc.RunAsGroup != nil {
		t.Fatalf("omitted user must stay privileged root: %+v", sc)
	}
}

func TestGenerateTemplateCreateRequestUsesMasterDownloadEndpointForS3Artifacts(t *testing.T) {
	req := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-s3"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "template-s3",
		WritableLayerSize: "20Gi",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
	}
	artifact := &models.RootfsArtifact{
		ArtifactID:              "artifact-s3",
		TemplateSpecFingerprint: "fingerprint-s3",
		Ext4SHA256:              "sha256-s3",
		Ext4SizeBytes:           2048,
		DownloadToken:           "token-s3",
		MasterNodeIP:            "http://0.0.0.0:8089",
		ArtifactURL:             "http://minio:9000/cube-volumes/artifact-s3.ext4?sig=stale",
	}
	got, err := generateTemplateCreateRequest(context.Background(), req, artifact, DockerImageConfig{}, "http://master.example")
	if err != nil {
		t.Fatalf("generateTemplateCreateRequest failed: %v", err)
	}
	want := "http://master.example/cube/template/artifact/download?artifact_id=artifact-s3&token=token-s3"
	if got.Containers[0].Image.Annotations[constants.CubeAnnotationRootfsArtifactURL] != want {
		t.Fatalf("artifact download url=%q, want %q", got.Containers[0].Image.Annotations[constants.CubeAnnotationRootfsArtifactURL], want)
	}
}

func TestGenerateTemplateCreateRequestFallsBackToArtifactRowForS3WhenNoExplicitBase(t *testing.T) {
	req := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-s3-row"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "template-s3-row",
		WritableLayerSize: "20Gi",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
	}
	artifact := &models.RootfsArtifact{
		ArtifactID:              "artifact-s3-row",
		TemplateSpecFingerprint: "fingerprint-s3-row",
		Ext4SHA256:              "sha256-s3-row",
		Ext4SizeBytes:           2048,
		DownloadToken:           "token-s3-row",
		MasterNodeIP:            "http://master-from-row:8089",
		ArtifactURL:             "http://minio:9000/cube-volumes/artifact-s3-row.ext4?sig=stale",
	}
	got, err := generateTemplateCreateRequest(context.Background(), req, artifact, DockerImageConfig{}, "")
	if err != nil {
		t.Fatalf("generateTemplateCreateRequest failed: %v", err)
	}
	want := "http://master-from-row:8089/cube/template/artifact/download?artifact_id=artifact-s3-row&token=token-s3-row"
	if got.Containers[0].Image.Annotations[constants.CubeAnnotationRootfsArtifactURL] != want {
		t.Fatalf("artifact download url=%q, want %q", got.Containers[0].Image.Annotations[constants.CubeAnnotationRootfsArtifactURL], want)
	}
}

func TestGenerateTemplateCreateRequestInjectsBlockIOQos(t *testing.T) {
	req := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-qos"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "template-qos",
		WritableLayerSize: "20Gi",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
		Qos:               &qos.Config{BlockIO: &qos.BlockIOConfig{ThroughputMiBps: 64, IOPS: 1000}},
	}
	artifact := &models.RootfsArtifact{
		ArtifactID:              "artifact-qos",
		TemplateSpecFingerprint: "fingerprint-qos",
		Ext4SHA256:              "sha256-qos",
		Ext4SizeBytes:           1024,
		DownloadToken:           "token-qos",
	}
	got, err := generateTemplateCreateRequest(context.Background(), req, artifact, DockerImageConfig{}, "http://master.example")
	if err != nil {
		t.Fatalf("generateTemplateCreateRequest failed: %v", err)
	}
	want := `{"bandwidth":{"size":67108864,"refill_time":1000},"ops":{"size":1000,"refill_time":1000}}`
	if got.Annotations[constants.CubeAnnotationsBlkQos] != want {
		t.Fatalf("block io qos annotation=%q, want %q", got.Annotations[constants.CubeAnnotationsBlkQos], want)
	}
	if _, ok := got.Annotations[constants.CubeAnnotationsNetWork]; ok {
		t.Fatal("block-io-only qos unexpectedly emitted network annotation")
	}
}

func TestGenerateTemplateCreateRequestInjectsNetworkQos(t *testing.T) {
	req := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-qos"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "template-qos",
		WritableLayerSize: "20Gi",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
		Qos:               &qos.Config{Network: &qos.NetworkConfig{BandwidthMbps: 100, PacketsPerSecond: 5000}},
	}
	artifact := &models.RootfsArtifact{
		ArtifactID:              "artifact-qos",
		TemplateSpecFingerprint: "fingerprint-qos",
		Ext4SHA256:              "sha256-qos",
		Ext4SizeBytes:           1024,
		DownloadToken:           "token-qos",
	}
	got, err := generateTemplateCreateRequest(context.Background(), req, artifact, DockerImageConfig{}, "http://master.example")
	if err != nil {
		t.Fatalf("generateTemplateCreateRequest failed: %v", err)
	}
	want := `{"Qos":{"BandWidth":{"Size":1250000,"OneTimeBurst":0,"RefillTime":100},"OPS":{"Size":5000,"OneTimeBurst":0,"RefillTime":1000}},"Version":1}`
	if got.Annotations[constants.CubeAnnotationsNetWork] != want {
		t.Fatalf("network qos annotation=%q, want %q", got.Annotations[constants.CubeAnnotationsNetWork], want)
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
	got, err := generateTemplateCreateRequest(context.Background(), req, artifact, DockerImageConfig{}, "http://master.example")
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
	maskRequestHost := "localhost:${PORT}"
	req := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "template-1",
		WritableLayerSize: "20Gi",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
		CubeNetworkConfig: &types.CubeNetworkConfig{
			AllowInternetAccess: &allowInternetAccess,
			MaskRequestHost:     &maskRequestHost,
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

	got, err := generateTemplateCreateRequest(context.Background(), req, artifact, DockerImageConfig{}, "http://master.example")
	require.NoError(t, err)
	require.NotNil(t, got.CubeNetworkConfig)
	require.NotNil(t, got.CubeNetworkConfig.MaskRequestHost)
	assert.Equal(t, maskRequestHost, *got.CubeNetworkConfig.MaskRequestHost)
	assert.NotSame(t, req.CubeNetworkConfig.MaskRequestHost, got.CubeNetworkConfig.MaskRequestHost)
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
	got, err := generateTemplateCreateRequest(context.Background(), req, artifact, DockerImageConfig{}, "http://master.example")
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

func TestTemplateInfoFromCommitJobIncludesConfiguredQos(t *testing.T) {
	network, err := qos.MarshalNetworkAnnotation(&qos.NetworkConfig{BandwidthMbps: 100, PacketsPerSecond: 5000})
	if err != nil {
		t.Fatalf("MarshalNetworkAnnotation failed: %v", err)
	}
	blockIO, err := qos.MarshalBlockIOAnnotation(&qos.BlockIOConfig{
		ThroughputMiBps: 64,
		IOPS:            1000,
	})
	if err != nil {
		t.Fatalf("MarshalBlockIOAnnotation failed: %v", err)
	}
	requestJSON, err := json.Marshal(&types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			constants.CubeAnnotationsNetWork: network,
			constants.CubeAnnotationsBlkQos:  blockIO,
		},
	})
	if err != nil {
		t.Fatalf("marshal commit request failed: %v", err)
	}

	info := templateInfoFromJob(&models.TemplateImageJob{
		TemplateID:  "tpl-commit-qos",
		Operation:   JobOperationCommit,
		RequestJSON: string(requestJSON),
	})
	if info.ConfiguredQos == nil || info.ConfiguredQos.Network == nil {
		t.Fatalf("configured qos=%+v, want network qos", info.ConfiguredQos)
	}
	if info.ConfiguredQos.Network.BandwidthMbps != 100 || info.ConfiguredQos.Network.PacketsPerSecond != 5000 {
		t.Fatalf("network qos=%+v, want 100 Mbps and 5000 packets/s", info.ConfiguredQos.Network)
	}
	if info.ConfiguredQos.BlockIO == nil || info.ConfiguredQos.BlockIO.ThroughputMiBps != 64 || info.ConfiguredQos.BlockIO.IOPS != 1000 {
		t.Fatalf("block io qos=%+v, want 64 MiB/s and 1000 IOPS", info.ConfiguredQos.BlockIO)
	}
}

func TestTemplateInfoFromCreateRedoJobIncludesConfiguredQos(t *testing.T) {
	requestJSON, err := json.Marshal(&types.CreateTemplateFromImageReq{
		Qos: &qos.Config{Network: &qos.NetworkConfig{BandwidthMbps: 100}},
	})
	if err != nil {
		t.Fatalf("marshal template create request failed: %v", err)
	}

	for _, operation := range []string{JobOperationCreate, JobOperationRedo} {
		t.Run(operation, func(t *testing.T) {
			info := templateInfoFromJob(&models.TemplateImageJob{
				TemplateID:  "tpl-" + strings.ToLower(operation),
				Operation:   operation,
				RequestJSON: string(requestJSON),
			})
			if info.ConfiguredQos == nil || info.ConfiguredQos.Network == nil || info.ConfiguredQos.Network.BandwidthMbps != 100 {
				t.Fatalf("configured qos=%+v, want 100 Mbps network qos", info.ConfiguredQos)
			}
		})
	}
}

func TestTemplateInfoFromLegacyJobHasNoConfiguredQos(t *testing.T) {
	requestJSON, err := json.Marshal(&types.CreateTemplateFromImageReq{})
	if err != nil {
		t.Fatalf("marshal legacy template request failed: %v", err)
	}
	info := templateInfoFromJob(&models.TemplateImageJob{
		TemplateID:  "tpl-legacy",
		Operation:   JobOperationLegacy,
		RequestJSON: string(requestJSON),
	})
	if info.ConfiguredQos != nil {
		t.Fatalf("configured qos=%+v, want nil for a legacy job", info.ConfiguredQos)
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

func TestValidateReusableRootfsArtifactFileAcceptsMatchingStat(t *testing.T) {
	data := []byte("artifact-data")
	path, _ := writeRootfsArtifactTestFile(t, data)

	err := validateReusableRootfsArtifactFile(&models.RootfsArtifact{
		ArtifactID:    "rfs-1",
		Ext4Path:      path,
		Ext4SHA256:    strings.Repeat("b", 64),
		Ext4SizeBytes: int64(len(data)),
	})
	if err != nil {
		t.Fatalf("validateReusableRootfsArtifactFile failed: %v", err)
	}
}

func TestValidateReusableRootfsArtifactFileRejectsMissingFile(t *testing.T) {
	err := validateReusableRootfsArtifactFile(&models.RootfsArtifact{
		ArtifactID:    "rfs-1",
		Ext4Path:      filepath.Join(t.TempDir(), "missing.ext4"),
		Ext4SHA256:    strings.Repeat("a", 64),
		Ext4SizeBytes: 1024,
	})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected missing file error, got %v", err)
	}
}

func TestValidateReusableRootfsArtifactFileRejectsSizeMismatch(t *testing.T) {
	path, _ := writeRootfsArtifactTestFile(t, []byte("artifact-data"))

	err := validateReusableRootfsArtifactFile(&models.RootfsArtifact{
		ArtifactID:    "rfs-1",
		Ext4Path:      path,
		Ext4SizeBytes: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("expected size mismatch error, got %v", err)
	}
}

func TestValidateReusableRootfsArtifactFileRejectsNonRegularFile(t *testing.T) {
	err := validateReusableRootfsArtifactFile(&models.RootfsArtifact{
		ArtifactID:    "rfs-1",
		Ext4Path:      t.TempDir(),
		Ext4SizeBytes: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("expected non-regular file error, got %v", err)
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

func writeRootfsArtifactTestFile(t *testing.T, data []byte) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "artifact.ext4")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	sum := sha256.Sum256(data)
	return path, hex.EncodeToString(sum[:])
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
	fallbackRoot := ArtifactFallbackStoreRootDir()
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

func TestResolveRedoTargetsMatchesFailedJFSHostFactGroup(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	facts := &node.HostFacts{CPUIDHash: "cpu-a", HostKernelRelease: "5.15.0"}
	patches.ApplyFunc(healthyTemplateNodes, func(instanceType string) []*node.Node {
		return []*node.Node{{InsID: "new-owner", IP: "10.0.0.2", Healthy: true, HostFacts: facts}}
	})
	variantID, _, ok := hostcompat.TemplateVariantID("tpl-a", facts.CPUIDHash, facts.HostKernelRelease)
	require.True(t, ok)

	targets, err := resolveRedoTargets(cubeboxv1.InstanceType_cubebox.String(), &types.RedoTemplateFromImageReq{
		TemplateID:        "tpl-a",
		DistributionScope: []string{"new-owner"},
		FailedOnly:        true,
	}, []models.TemplateReplica{{
		NodeID:            "old-owner",
		NodeIP:            "10.0.0.1",
		TemplateVariantID: variantID,
		Status:            ReplicaStatusFailed,
	}})
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, "new-owner", targets[0].ID())
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

func TestEnsureRedoTemplateNotInUseScopesNonJFSToTargetNodes(t *testing.T) {
	nodeA := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: true}
	nodeB := &node.Node{InsID: "node-b", IP: "10.0.0.2", Healthy: true}
	nodeC := &node.Node{InsID: "node-c", IP: "10.0.0.3", Healthy: true}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(localcache.GetHealthyNodesByInstanceType, func(int, string) node.NodeList {
		return node.NodeList{nodeA, nodeB, nodeC}
	})

	origList := listRedoTemplateSandboxes
	t.Cleanup(func() { listRedoTemplateSandboxes = origList })

	var mu sync.Mutex
	listCalls := 0
	var seenHostIDs []string
	var listRsp *types.ListCubeSandboxRes
	sandboxes := []*types.SandboxBriefData{
		{SandboxID: "sb-a", TemplateID: "tpl-1", HostID: "node-a", HostIP: "10.0.0.1"},
		{SandboxID: "sb-other", TemplateID: "tpl-other", HostID: "node-b", HostIP: "10.0.0.2"},
	}
	listRedoTemplateSandboxes = func(_ context.Context, req *types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
		mu.Lock()
		defer mu.Unlock()
		listCalls++
		if req != nil {
			seenHostIDs = append(seenHostIDs, req.HostID)
		}
		if listRsp != nil {
			return listRsp
		}
		return &types.ListCubeSandboxRes{
			Ret:  &types.Ret{RetCode: int(errorcode.ErrorCode_Success)},
			Data: sandboxes,
		}
	}

	// Non-JFS redo targeting only node-b (no running sandbox on node-b) is allowed
	// and queries node-b specifically via HostID.
	err := ensureRedoTemplateNotInUse(context.Background(), "tpl-1", "cubebox", "xfs", false, []*node.Node{nodeB})
	require.NoError(t, err)
	require.Equal(t, 1, listCalls)
	require.Equal(t, []string{"node-b"}, seenHostIDs)

	// Non-JFS redo targeting newly added node-c (no running sandbox on node-c) is allowed.
	seenHostIDs = nil
	err = ensureRedoTemplateNotInUse(context.Background(), "tpl-1", "cubebox", "xfs", false, []*node.Node{nodeC})
	require.NoError(t, err)
	require.Equal(t, 2, listCalls)
	require.Equal(t, []string{"node-c"}, seenHostIDs)

	// Non-JFS redo targeting node-a (has running sandbox) is rejected.
	seenHostIDs = nil
	err = ensureRedoTemplateNotInUse(context.Background(), "tpl-1", "cubebox", "xfs", false, []*node.Node{nodeA})
	require.ErrorIs(t, err, ErrTemplateInUse)
	require.Equal(t, []string{"node-a"}, seenHostIDs)

	// Non-JFS redo targeting both node-a and node-b is rejected.
	err = ensureRedoTemplateNotInUse(context.Background(), "tpl-1", "cubebox", "xfs", false, []*node.Node{nodeA, nodeB})
	require.ErrorIs(t, err, ErrTemplateInUse)

	// HostIP-only sandbox match still blocks target node.
	sandboxes[0] = &types.SandboxBriefData{SandboxID: "sb-a-ip", TemplateID: "tpl-1", HostIP: "10.0.0.1"}
	err = ensureRedoTemplateNotInUse(context.Background(), "tpl-1", "cubebox", "s3", false, []*node.Node{nodeA})
	require.ErrorIs(t, err, ErrTemplateInUse)

	// Non-JFS redo that needs a full rebuild checks cluster-wide (HostID == "") even when targeting node-b.
	seenHostIDs = nil
	err = ensureRedoTemplateNotInUse(context.Background(), "tpl-1", "cubebox", "xfs", true, []*node.Node{nodeB})
	require.ErrorIs(t, err, ErrTemplateInUse)
	require.Equal(t, []string{""}, seenHostIDs)

	// JFS redo checks cluster-wide (HostID == "") even when targeting node-b.
	seenHostIDs = nil
	err = ensureRedoTemplateNotInUse(context.Background(), "tpl-1", "cubebox", "jfs", false, []*node.Node{nodeB})
	require.ErrorIs(t, err, ErrTemplateInUse)
	require.Equal(t, []string{""}, seenHostIDs)

	// Target Cubelet list RPC failure fails closed instead of silently treating as 0 sandboxes.
	listRsp = &types.ListCubeSandboxRes{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_ReqCubeAPIFailed),
			RetMsg:  "list sandbox failed: cubelet unreachable",
		},
	}
	err = ensureRedoTemplateNotInUse(context.Background(), "tpl-1", "cubebox", "xfs", false, []*node.Node{nodeB})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrTemplateInUse)
	require.Contains(t, err.Error(), "list sandbox failed: cubelet unreachable")
	require.NotContains(t, err.Error(), "list sandbox failed: list sandbox failed:")

	// Empty / nil response from listRedoTemplateSandboxes also fails closed.
	emptyRspFn := listRedoTemplateSandboxes
	listRedoTemplateSandboxes = func(_ context.Context, _ *types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
		return nil
	}
	err = ensureRedoTemplateNotInUse(context.Background(), "tpl-1", "cubebox", "xfs", false, []*node.Node{nodeB})
	require.Error(t, err)
	require.Contains(t, err.Error(), "list sandbox returned empty response")
	listRedoTemplateSandboxes = emptyRspFn
	listRsp = nil

	// Duplicate target nodes are deduplicated to a single HostID query.
	seenHostIDs = nil
	err = ensureRedoTemplateNotInUse(context.Background(), "tpl-1", "cubebox", "xfs", false, []*node.Node{nodeB, nodeB})
	require.NoError(t, err)
	require.Equal(t, []string{"node-b"}, seenHostIDs)

	// Non-empty slice with only nil / empty target nodes short-circuits without querying.
	seenHostIDs = nil
	callsBefore := listCalls
	err = ensureRedoTemplateNotInUse(context.Background(), "tpl-1", "cubebox", "xfs", false, []*node.Node{nil, {}})
	require.NoError(t, err)
	require.Equal(t, callsBefore, listCalls)

	// IP-only target node (empty InsID): Node.ID() returns IP, so it still queries
	// via HostID="10.0.0.1" and matches sandboxes on that HostIP.
	seenHostIDs = nil
	ipOnlyTarget := &node.Node{IP: "10.0.0.1", Healthy: true}
	err = ensureRedoTemplateNotInUse(context.Background(), "tpl-1", "cubebox", "xfs", false, []*node.Node{ipOnlyTarget})
	require.ErrorIs(t, err, ErrTemplateInUse)
	require.Equal(t, []string{"10.0.0.1"}, seenHostIDs)

	// Sandbox matched only via AppSnapshotTemplateID annotation blocks target node.
	sandboxes[0] = &types.SandboxBriefData{
		SandboxID: "sb-a-label",
		HostID:    "node-a",
		HostIP:    "10.0.0.1",
		Labels:    map[string]string{constants.CubeAnnotationAppSnapshotTemplateID: "tpl-1"},
	}
	err = ensureRedoTemplateNotInUse(context.Background(), "tpl-1", "cubebox", "xfs", false, []*node.Node{nodeA})
	require.ErrorIs(t, err, ErrTemplateInUse)
}

func TestResolveRedoAffectedNodesAndJFSHostFactGroups(t *testing.T) {
	nodeA1 := &node.Node{
		InsID:     "node-a1",
		IP:        "10.0.0.1",
		Healthy:   true,
		HostFacts: &node.HostFacts{CPUIDHash: "cpu-a", HostKernelRelease: "5.15.0"},
	}
	nodeA2 := &node.Node{
		InsID:     "node-a2",
		IP:        "10.0.0.2",
		Healthy:   true,
		HostFacts: &node.HostFacts{CPUIDHash: "cpu-a", HostKernelRelease: "5.15.0"},
	}
	nodeB1 := &node.Node{
		InsID:     "node-b1",
		IP:        "10.0.0.3",
		Healthy:   true,
		HostFacts: &node.HostFacts{CPUIDHash: "cpu-b", HostKernelRelease: "6.6.0"},
	}
	nodeNoFacts := &node.Node{InsID: "node-nofacts", IP: "10.0.0.4", Healthy: true}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(localcache.GetHealthyNodesByInstanceType, func(int, string) node.NodeList {
		return node.NodeList{nodeA1, nodeA2, nodeB1}
	})

	// Non-JFS reuse affects only requested targets; empty targets means 0 nodes (scoped=true).
	affected, scoped := resolveRedoAffectedNodes("tpl-1", "cubebox", "xfs", false, []*node.Node{nodeB1})
	require.True(t, scoped)
	require.Equal(t, []*node.Node{nodeB1}, affected)
	affected, scoped = resolveRedoAffectedNodes("tpl-1", "cubebox", "xfs", false, []*node.Node{})
	require.True(t, scoped)
	require.Empty(t, affected)

	// Full rebuild always checks cluster-wide (scoped=false).
	affected, scoped = resolveRedoAffectedNodes("tpl-1", "cubebox", "jfs", true, []*node.Node{nodeB1})
	require.False(t, scoped)
	require.Nil(t, affected)
	affected, scoped = resolveRedoAffectedNodes("tpl-1", "cubebox", "xfs", true, []*node.Node{nodeB1})
	require.False(t, scoped)
	require.Nil(t, affected)

	// JFS targeting nodeB1 affects only HostFact group B ([nodeB1], scoped=true).
	affected, scoped = resolveRedoAffectedNodes("tpl-1", "cubebox", "jfs", false, []*node.Node{nodeB1})
	require.True(t, scoped)
	require.Equal(t, []*node.Node{nodeB1}, affected)

	// JFS targeting nodeA2 expands to entire HostFact group A ([nodeA2, nodeA1], scoped=true).
	affected, scoped = resolveRedoAffectedNodes("tpl-1", "cubebox", "jfs", false, []*node.Node{nodeA2})
	require.True(t, scoped)
	require.Equal(t, []*node.Node{nodeA2, nodeA1}, affected)

	// JFS targeting a node without HostFacts falls back conservatively to cluster-wide (scoped=false).
	affected, scoped = resolveRedoAffectedNodes("tpl-1", "cubebox", "jfs", false, []*node.Node{nodeNoFacts})
	require.False(t, scoped)
	require.Nil(t, affected)
	// Empty JFS target list also falls back conservatively to cluster-wide (scoped=false).
	affected, scoped = resolveRedoAffectedNodes("tpl-1", "cubebox", "jfs", false, nil)
	require.False(t, scoped)
	require.Nil(t, affected)

	var mu sync.Mutex
	var seenHostIDs []string
	origList := listRedoTemplateSandboxes
	t.Cleanup(func() { listRedoTemplateSandboxes = origList })
	listRedoTemplateSandboxes = func(_ context.Context, req *types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
		if req != nil {
			mu.Lock()
			seenHostIDs = append(seenHostIDs, req.HostID)
			mu.Unlock()
			if req.HostID == "node-b1" || req.HostID == "node-a2" {
				return &types.ListCubeSandboxRes{
					Ret:  &types.Ret{RetCode: int(errorcode.ErrorCode_Success)},
					Data: nil,
				}
			}
		}
		return &types.ListCubeSandboxRes{
			Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)},
			Data: []*types.SandboxBriefData{
				{SandboxID: "sb-a1", TemplateID: "tpl-1", HostID: "node-a1", HostIP: "10.0.0.1"},
			},
		}
	}

	// Sandbox running on nodeA1 (group A) does NOT block JFS redo targeting nodeB1 (group B).
	seenHostIDs = nil
	require.NoError(t, ensureRedoTemplateNotInUse(context.Background(), "tpl-1", "cubebox", "jfs", false, []*node.Node{nodeB1}))
	require.Equal(t, []string{"node-b1"}, seenHostIDs)

	// Sandbox running on nodeA1 (group A) DOES block JFS redo targeting peer nodeA2 in group A.
	seenHostIDs = nil
	err := ensureRedoTemplateNotInUse(context.Background(), "tpl-1", "cubebox", "jfs", false, []*node.Node{nodeA2})
	require.ErrorIs(t, err, ErrTemplateInUse)
	require.Contains(t, seenHostIDs, "node-a1")
	require.Subset(t, []string{"node-a2", "node-a1"}, seenHostIDs)

	// JFS target without HostFacts falls back to cluster-wide scan and rejects.
	seenHostIDs = nil
	err = ensureRedoTemplateNotInUse(context.Background(), "tpl-1", "cubebox", "jfs", false, []*node.Node{nodeNoFacts})
	require.ErrorIs(t, err, ErrTemplateInUse)
	require.Equal(t, []string{""}, seenHostIDs)
}

func TestSubmitRedoTemplateFromImageScopedInUseAndFullRebuild(t *testing.T) {
	newTestDB := func(t *testing.T) *gorm.DB {
		t.Helper()
		dbName := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
		db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		require.NoError(t, err)
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, db.AutoMigrate(&models.TemplateDefinition{}, &models.TemplateImageJob{}, &models.TemplateReplica{}, &models.RootfsArtifact{}))
		oldDB := store.db
		store.db = db
		t.Cleanup(func() {
			store.db = oldDB
			_ = sqlDB.Close()
		})
		return db
	}

	setupRedoCluster := func(t *testing.T, patches *gomonkey.Patches, nodes []*node.Node, listFn func(*types.ListCubeSandboxReq) *types.ListCubeSandboxRes) *[]string {
		t.Helper()
		patches.ApplyFunc(healthyTemplateNodes, func(string) []*node.Node { return nodes })
		patches.ApplyFunc(localcache.GetHealthyNodesByInstanceType, func(int, string) node.NodeList { return nodes })
		var mu sync.Mutex
		var seenHostIDs []string
		origList := listRedoTemplateSandboxes
		t.Cleanup(func() { listRedoTemplateSandboxes = origList })
		listRedoTemplateSandboxes = func(_ context.Context, req *types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
			if req != nil {
				mu.Lock()
				seenHostIDs = append(seenHostIDs, req.HostID)
				mu.Unlock()
			}
			return listFn(req)
		}
		return &seenHostIDs
	}

	seedRedoTemplate := func(t *testing.T, db *gorm.DB, tplID, defStatus, backend, artStatus string, replicas ...models.TemplateReplica) {
		t.Helper()
		reqJSON, err := marshalTemplateImageJobRequest(&types.CreateTemplateFromImageReq{
			SourceImageRef:    "docker.io/library/nginx:latest",
			TemplateID:        tplID,
			WritableLayerSize: "20Gi",
			InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
			Backend:           backend,
		})
		require.NoError(t, err)
		require.NoError(t, db.Create(&models.TemplateDefinition{
			TemplateID:     tplID,
			InstanceType:   cubeboxv1.InstanceType_cubebox.String(),
			Status:         defStatus,
			StorageBackend: backend,
			RequestJSON:    "{}",
		}).Error)
		artID := "art-" + tplID
		require.NoError(t, db.Create(&models.RootfsArtifact{
			ArtifactID: artID,
			Status:     artStatus,
		}).Error)
		require.NoError(t, db.Create(&models.TemplateImageJob{
			JobID:       "job-" + tplID + "-1",
			TemplateID:  tplID,
			RequestID:   "req-" + tplID + "-1",
			AttemptNo:   1,
			Operation:   JobOperationCreate,
			ArtifactID:  artID,
			Status:      JobStatusReady,
			Phase:       JobPhaseReady,
			RequestJSON: reqJSON,
		}).Error)
		for _, r := range replicas {
			r.TemplateID = tplID
			require.NoError(t, db.Create(&r).Error)
		}
	}

	sandboxOnNodeA := func(tplID string) func(*types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
		return func(req *types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
			if req != nil && req.HostID == "node-b" {
				return &types.ListCubeSandboxRes{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)}}
			}
			return &types.ListCubeSandboxRes{
				Ret:  &types.Ret{RetCode: int(errorcode.ErrorCode_Success)},
				Data: []*types.SandboxBriefData{{SandboxID: "sb-a", TemplateID: tplID, HostID: "node-a", HostIP: "10.0.0.1"}},
			}
		}
	}

	nodeA := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: true}
	nodeB := &node.Node{InsID: "node-b", IP: "10.0.0.2", Healthy: true}
	nodeC := &node.Node{InsID: "node-c", IP: "10.0.0.3", Healthy: true}

	t.Run("partially ready failed_only allowed when sandbox runs on ready node", func(t *testing.T) {
		db := newTestDB(t)
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		setupRedoCluster(t, patches, []*node.Node{nodeA, nodeB}, sandboxOnNodeA("tpl-partial"))
		redoLaunched := make(chan struct{}, 1)
		patches.ApplyFunc(runRedoTemplateImageJob, func(context.Context, string, *types.RedoTemplateFromImageReq, string) {
			redoLaunched <- struct{}{}
		})
		seedRedoTemplate(t, db, "tpl-partial", StatusPartiallyReady, "xfs", ArtifactStatusReady,
			models.TemplateReplica{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
			models.TemplateReplica{NodeID: "node-b", NodeIP: "10.0.0.2", Status: ReplicaStatusFailed},
		)

		info, err := SubmitRedoTemplateFromImage(context.Background(), &types.RedoTemplateFromImageReq{
			Request:    &types.Request{RequestID: "req-redo-partial"},
			TemplateID: "tpl-partial",
			FailedOnly: true,
		}, "http://master.example")
		require.NoError(t, err)
		require.Equal(t, RedoModeFailedOnly, info.RedoMode)
		require.Equal(t, []string{"node-b"}, info.RedoScope)
		select {
		case <-redoLaunched:
		case <-time.After(2 * time.Second):
			t.Fatal("expected runRedoTemplateImageJob to be invoked")
		}
	})

	t.Run("explicit redo to new node allowed while sandbox runs on existing node", func(t *testing.T) {
		db := newTestDB(t)
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		setupRedoCluster(t, patches, []*node.Node{nodeA, nodeB, nodeC}, sandboxOnNodeA("tpl-newnode"))
		redoLaunched := make(chan struct{}, 1)
		patches.ApplyFunc(runRedoTemplateImageJob, func(context.Context, string, *types.RedoTemplateFromImageReq, string) {
			redoLaunched <- struct{}{}
		})
		seedRedoTemplate(t, db, "tpl-newnode", StatusReady, "xfs", ArtifactStatusReady,
			models.TemplateReplica{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
		)

		info, err := SubmitRedoTemplateFromImage(context.Background(), &types.RedoTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-redo-newnode"},
			TemplateID:        "tpl-newnode",
			DistributionScope: []string{"node-c"},
		}, "http://master.example")
		require.NoError(t, err)
		require.Equal(t, RedoModeNodes, info.RedoMode)
		require.Equal(t, []string{"node-c"}, info.RedoScope)
		select {
		case <-redoLaunched:
		case <-time.After(2 * time.Second):
			t.Fatal("expected runRedoTemplateImageJob to be invoked")
		}
	})

	t.Run("partially ready scoped to ready node in use is rejected", func(t *testing.T) {
		db := newTestDB(t)
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		setupRedoCluster(t, patches, []*node.Node{nodeA, nodeB}, sandboxOnNodeA("tpl-blocked"))
		seedRedoTemplate(t, db, "tpl-blocked", StatusPartiallyReady, "xfs", ArtifactStatusReady,
			models.TemplateReplica{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
		)

		_, err := SubmitRedoTemplateFromImage(context.Background(), &types.RedoTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-redo-blocked"},
			TemplateID:        "tpl-blocked",
			DistributionScope: []string{"node-a"},
		}, "http://master.example")
		require.ErrorIs(t, err, ErrTemplateInUse)
	})

	t.Run("full rebuild checks cluster-wide even when targeting failed node", func(t *testing.T) {
		db := newTestDB(t)
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		setupRedoCluster(t, patches, []*node.Node{nodeA, nodeB}, sandboxOnNodeA("tpl-rebuild"))
		seedRedoTemplate(t, db, "tpl-rebuild", StatusPartiallyReady, "xfs", ArtifactStatusFailed,
			models.TemplateReplica{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
			models.TemplateReplica{NodeID: "node-b", NodeIP: "10.0.0.2", Status: ReplicaStatusFailed},
		)

		_, err := SubmitRedoTemplateFromImage(context.Background(), &types.RedoTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-redo-rebuild"},
			TemplateID:        "tpl-rebuild",
			DistributionScope: []string{"node-b"},
		}, "http://master.example")
		require.ErrorIs(t, err, ErrTemplateInUse)
	})

	t.Run("missing definition is rejected with ErrTemplateNotFound", func(t *testing.T) {
		db := newTestDB(t)
		reqJSON, err := marshalTemplateImageJobRequest(&types.CreateTemplateFromImageReq{
			SourceImageRef:    "docker.io/library/nginx:latest",
			TemplateID:        "tpl-no-def",
			WritableLayerSize: "20Gi",
			InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
			Backend:           "xfs",
		})
		require.NoError(t, err)
		require.NoError(t, db.Create(&models.TemplateImageJob{
			JobID:       "job-nodef-1",
			TemplateID:  "tpl-no-def",
			RequestID:   "req-nodef-1",
			AttemptNo:   1,
			Operation:   JobOperationCreate,
			Status:      JobStatusFailed,
			Phase:       JobPhaseDistributing,
			RequestJSON: reqJSON,
		}).Error)

		_, err = SubmitRedoTemplateFromImage(context.Background(), &types.RedoTemplateFromImageReq{
			Request:    &types.Request{RequestID: "req-redo-nodef"},
			TemplateID: "tpl-no-def",
		}, "http://master.example")
		require.ErrorIs(t, err, ErrTemplateNotFound)
	})

	t.Run("jfs_backend_without_host_facts_checks_cluster-wide", func(t *testing.T) {
		db := newTestDB(t)
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		seenHostIDs := setupRedoCluster(t, patches, []*node.Node{nodeA, nodeB}, sandboxOnNodeA("tpl-jfs"))
		seedRedoTemplate(t, db, "tpl-jfs", StatusPartiallyReady, "jfs", ArtifactStatusReady,
			models.TemplateReplica{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
			models.TemplateReplica{NodeID: "node-b", NodeIP: "10.0.0.2", Status: ReplicaStatusFailed},
		)

		_, err := SubmitRedoTemplateFromImage(context.Background(), &types.RedoTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-redo-jfs"},
			TemplateID:        "tpl-jfs",
			DistributionScope: []string{"node-b"},
		}, "http://master.example")
		require.ErrorIs(t, err, ErrTemplateInUse)
		require.Equal(t, []string{""}, *seenHostIDs)
	})

	t.Run("jfs_redo_to_different_host_fact_group_allowed_while_sandbox_runs_in_other_group", func(t *testing.T) {
		db := newTestDB(t)
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		nodeGroupA := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: true, HostFacts: &node.HostFacts{CPUIDHash: "cpu-a", HostKernelRelease: "5.15.0"}}
		nodeGroupB := &node.Node{InsID: "node-b", IP: "10.0.0.2", Healthy: true, HostFacts: &node.HostFacts{CPUIDHash: "cpu-b", HostKernelRelease: "6.6.0"}}
		seenHostIDs := setupRedoCluster(t, patches, []*node.Node{nodeGroupA, nodeGroupB}, sandboxOnNodeA("tpl-jfs-groups"))
		redoLaunched := make(chan struct{}, 1)
		patches.ApplyFunc(runRedoTemplateImageJob, func(context.Context, string, *types.RedoTemplateFromImageReq, string) {
			redoLaunched <- struct{}{}
		})
		seedRedoTemplate(t, db, "tpl-jfs-groups", StatusPartiallyReady, "jfs", ArtifactStatusReady,
			models.TemplateReplica{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
		)

		info, err := SubmitRedoTemplateFromImage(context.Background(), &types.RedoTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-redo-jfs-group-b"},
			TemplateID:        "tpl-jfs-groups",
			DistributionScope: []string{"node-b"},
		}, "http://master.example")
		require.NoError(t, err)
		require.Equal(t, []string{"node-b"}, info.RedoScope)
		require.Equal(t, []string{"node-b"}, *seenHostIDs)
		select {
		case <-redoLaunched:
		case <-time.After(2 * time.Second):
			t.Fatal("expected runRedoTemplateImageJob to be invoked")
		}
	})

	t.Run("jfs_redo_to_peer_node_in_same_host_fact_group_rejected_when_group_in_use", func(t *testing.T) {
		db := newTestDB(t)
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		nodeGroupA1 := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: true, HostFacts: &node.HostFacts{CPUIDHash: "cpu-a", HostKernelRelease: "5.15.0"}}
		nodeGroupA2 := &node.Node{InsID: "node-b", IP: "10.0.0.2", Healthy: true, HostFacts: &node.HostFacts{CPUIDHash: "cpu-a", HostKernelRelease: "5.15.0"}}
		seenHostIDs := setupRedoCluster(t, patches, []*node.Node{nodeGroupA1, nodeGroupA2}, sandboxOnNodeA("tpl-jfs-same-group"))
		seedRedoTemplate(t, db, "tpl-jfs-same-group", StatusPartiallyReady, "jfs", ArtifactStatusReady,
			models.TemplateReplica{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
		)

		_, err := SubmitRedoTemplateFromImage(context.Background(), &types.RedoTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-redo-jfs-same-group"},
			TemplateID:        "tpl-jfs-same-group",
			DistributionScope: []string{"node-b"},
		}, "http://master.example")
		require.ErrorIs(t, err, ErrTemplateInUse)
		require.Contains(t, *seenHostIDs, "node-a")
		require.Subset(t, []string{"node-b", "node-a"}, *seenHostIDs)
	})

	t.Run("target_node_cubelet_rpc_failure_fails_closed_and_rolls_back_job", func(t *testing.T) {
		db := newTestDB(t)
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		seenHostIDs := setupRedoCluster(t, patches, []*node.Node{nodeA, nodeB}, func(*types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
			return &types.ListCubeSandboxRes{
				Ret: &types.Ret{
					RetCode: int(errorcode.ErrorCode_ReqCubeAPIFailed),
					RetMsg:  "list sandbox failed: cubelet unreachable",
				},
			}
		})
		seedRedoTemplate(t, db, "tpl-rpc-fail", StatusPartiallyReady, "xfs", ArtifactStatusReady,
			models.TemplateReplica{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
			models.TemplateReplica{NodeID: "node-b", NodeIP: "10.0.0.2", Status: ReplicaStatusFailed},
		)

		_, err := SubmitRedoTemplateFromImage(context.Background(), &types.RedoTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-redo-rpc-fail"},
			TemplateID:        "tpl-rpc-fail",
			DistributionScope: []string{"node-b"},
		}, "http://master.example")
		require.Error(t, err)
		require.Contains(t, err.Error(), "list sandbox failed: cubelet unreachable")
		require.Equal(t, []string{"node-b"}, *seenHostIDs)

		var redoCount int64
		require.NoError(t, db.Table(constants.TemplateImageJobTableName).
			Where("template_id = ? AND operation = ?", "tpl-rpc-fail", JobOperationRedo).
			Count(&redoCount).Error)
		require.Zero(t, redoCount)
	})

	t.Run("cubelet_rpc_runs_outside_template_write_lock", func(t *testing.T) {
		db := newTestDB(t)
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		lockFree := false
		setupRedoCluster(t, patches, []*node.Node{nodeA, nodeB}, func(*types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
			lock := templateRequestLockGroup.get("tpl-lock-free")
			if lock != nil && lock.TryRLock() {
				lockFree = true
				lock.RUnlock()
			}
			return &types.ListCubeSandboxRes{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)}}
		})
		redoLaunched := make(chan struct{}, 1)
		patches.ApplyFunc(runRedoTemplateImageJob, func(context.Context, string, *types.RedoTemplateFromImageReq, string) {
			redoLaunched <- struct{}{}
		})
		seedRedoTemplate(t, db, "tpl-lock-free", StatusPartiallyReady, "xfs", ArtifactStatusReady,
			models.TemplateReplica{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
			models.TemplateReplica{NodeID: "node-b", NodeIP: "10.0.0.2", Status: ReplicaStatusFailed},
		)

		info, err := SubmitRedoTemplateFromImage(context.Background(), &types.RedoTemplateFromImageReq{
			Request:    &types.Request{RequestID: "req-redo-lock-free"},
			TemplateID: "tpl-lock-free",
			FailedOnly: true,
		}, "http://master.example")
		require.NoError(t, err)
		require.False(t, info.NeedsFullRebuild)
		require.True(t, lockFree, "template write lock must not be held while running Cubelet ListSandbox RPCs")
		select {
		case <-redoLaunched:
		case <-time.After(2 * time.Second):
			t.Fatal("expected runRedoTemplateImageJob to be invoked")
		}
	})

	t.Run("concurrent_scope_widening_between_preflight_and_lock_rejected_without_second_rpc", func(t *testing.T) {
		db := newTestDB(t)
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		seenHostIDs := setupRedoCluster(t, patches, []*node.Node{nodeA, nodeB}, func(*types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
			// Simulate concurrent artifact invalidation before SubmitRedoTemplateFromImage enters the write lock.
			require.NoError(t, db.Table(constants.RootfsArtifactTableName).
				Where("artifact_id = ?", "art-tpl-race").
				Update("status", ArtifactStatusFailed).Error)
			return &types.ListCubeSandboxRes{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)}}
		})
		seedRedoTemplate(t, db, "tpl-race", StatusPartiallyReady, "xfs", ArtifactStatusReady,
			models.TemplateReplica{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
			models.TemplateReplica{NodeID: "node-b", NodeIP: "10.0.0.2", Status: ReplicaStatusFailed},
		)

		_, err := SubmitRedoTemplateFromImage(context.Background(), &types.RedoTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-redo-race"},
			TemplateID:        "tpl-race",
			DistributionScope: []string{"node-b"},
		}, "http://master.example")
		require.ErrorIs(t, err, ErrRedoTemplateScopeChanged)
		require.NotErrorIs(t, err, ErrTemplateAttemptInProgress)
		require.Equal(t, []string{"node-b"}, *seenHostIDs)
	})

	t.Run("resume_phase_building_ext4_with_ready_artifact_requires_full_rebuild", func(t *testing.T) {
		db := newTestDB(t)
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		seenHostIDs := setupRedoCluster(t, patches, []*node.Node{nodeA, nodeB}, func(*types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
			return &types.ListCubeSandboxRes{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)}}
		})
		patches.ApplyFunc(runRedoTemplateImageJob, func(context.Context, string, *types.RedoTemplateFromImageReq, string) {
			t.Fatal("runRedoTemplateImageJob must not run locally when ResumePhase requires full rebuild")
		})
		seedRedoTemplate(t, db, "tpl-ext4-ready", StatusFailed, "xfs", ArtifactStatusReady,
			models.TemplateReplica{NodeID: "node-a", NodeIP: "10.0.0.1", Status: ReplicaStatusReady},
			models.TemplateReplica{NodeID: "node-b", NodeIP: "10.0.0.2", Status: ReplicaStatusFailed},
		)
		require.NoError(t, db.Table(constants.TemplateImageJobTableName).
			Where("job_id = ?", "job-tpl-ext4-ready-1").
			Updates(map[string]any{
				"status": JobStatusFailed,
				"phase":  JobPhaseBuildingExt4,
			}).Error)

		info, err := SubmitRedoTemplateFromImage(context.Background(), &types.RedoTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-redo-ext4-ready"},
			TemplateID:        "tpl-ext4-ready",
			DistributionScope: []string{"node-b"},
		}, "http://master.example")
		require.NoError(t, err)
		require.Equal(t, JobStatusPending, info.Status)
		require.Equal(t, JobPhaseBuildingExt4, info.ResumePhase)
		require.True(t, info.NeedsFullRebuild)
		require.True(t, RedoNeedsFullRebuild(context.Background(), info.JobID))
		require.Equal(t, []string{""}, *seenHostIDs)
	})
}

func TestPrepareRedoTemplateReplicasScopedInUse(t *testing.T) {
	nodeA := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: true}
	nodeB := &node.Node{InsID: "node-b", IP: "10.0.0.2", Healthy: true}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(localcache.GetHealthyNodesByInstanceType, func(int, string) node.NodeList {
		return node.NodeList{nodeA, nodeB}
	})

	cleanupCalls := 0
	patches.ApplyFunc(cleanupTemplateReplicasOnNodes, func(context.Context, string, []models.TemplateReplica, []*node.Node, string) error {
		cleanupCalls++
		return nil
	})
	patches.ApplyFunc(markJFSRedoReplicaRowsUnavailable, func(context.Context, string, []*node.Node) error { return nil })
	patches.ApplyFunc(deleteJFSRedoReplicaRows, func(context.Context, string, []*node.Node) error { return nil })

	var mu sync.Mutex
	var seenHostIDs []string
	origList := listRedoTemplateSandboxes
	t.Cleanup(func() { listRedoTemplateSandboxes = origList })
	listRedoTemplateSandboxes = func(_ context.Context, req *types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
		if req != nil {
			mu.Lock()
			seenHostIDs = append(seenHostIDs, req.HostID)
			mu.Unlock()
			if req.HostID == "node-b" {
				return &types.ListCubeSandboxRes{
					Ret:  &types.Ret{RetCode: int(errorcode.ErrorCode_Success)},
					Data: nil,
				}
			}
		}
		return &types.ListCubeSandboxRes{
			Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)},
			Data: []*types.SandboxBriefData{
				{SandboxID: "sb-a", TemplateID: "tpl-prep", HostID: "node-a", HostIP: "10.0.0.1"},
			},
		}
	}

	// Non-JFS xfs targeting node-b passes in-use check and cleans up node-b.
	require.NoError(t, prepareRedoTemplateReplicas(context.Background(), "tpl-prep", "cubebox", nil, []*node.Node{nodeB}, "xfs"))
	require.Equal(t, []string{"node-b"}, seenHostIDs)
	require.Equal(t, 1, cleanupCalls)

	// Non-JFS xfs targeting node-a (has sandbox) is rejected before cleanup.
	seenHostIDs = nil
	err := prepareRedoTemplateReplicas(context.Background(), "tpl-prep", "cubebox", nil, []*node.Node{nodeA}, "xfs")
	require.ErrorIs(t, err, ErrTemplateInUse)
	require.Equal(t, []string{"node-a"}, seenHostIDs)
	require.Equal(t, 1, cleanupCalls)

	// JFS targeting node-b without HostFacts falls back to cluster-wide (HostID == "") and rejects before cleanup.
	seenHostIDs = nil
	err = prepareRedoTemplateReplicas(context.Background(), "tpl-prep", "cubebox", nil, []*node.Node{nodeB}, "jfs")
	require.ErrorIs(t, err, ErrTemplateInUse)
	require.Equal(t, []string{""}, seenHostIDs)
	require.Equal(t, 1, cleanupCalls)

	// JFS targeting node-b in a different HostFact group is allowed even while node-a has a running sandbox.
	nodeA.HostFacts = &node.HostFacts{CPUIDHash: "cpu-a", HostKernelRelease: "5.15.0"}
	nodeB.HostFacts = &node.HostFacts{CPUIDHash: "cpu-b", HostKernelRelease: "6.6.0"}
	seenHostIDs = nil
	require.NoError(t, prepareRedoTemplateReplicas(context.Background(), "tpl-prep", "cubebox", nil, []*node.Node{nodeB}, "jfs"))
	require.Equal(t, []string{"node-b"}, seenHostIDs)
	require.Equal(t, 2, cleanupCalls)

	// JFS targeting node-b in the SAME HostFact group as node-a expands to [node-b, node-a] and rejects before cleanup.
	nodeB.HostFacts = &node.HostFacts{CPUIDHash: "cpu-a", HostKernelRelease: "5.15.0"}
	seenHostIDs = nil
	err = prepareRedoTemplateReplicas(context.Background(), "tpl-prep", "cubebox", nil, []*node.Node{nodeB}, "jfs")
	require.ErrorIs(t, err, ErrTemplateInUse)
	require.Contains(t, seenHostIDs, "node-a")
	require.Subset(t, []string{"node-b", "node-a"}, seenHostIDs)
	require.Equal(t, 2, cleanupCalls)
}

func TestRunRedoTemplateImageJobBuildingExt4ChecksClusterWideInUseBeforeDestructivePrepare(t *testing.T) {
	nodeA := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: true}
	nodeB := &node.Node{InsID: "node-b", IP: "10.0.0.2", Healthy: true}

	patches := gomonkey.NewPatches()
	defer patches.Reset()

	patches.ApplyFunc(localcache.GetHealthyNodesByInstanceType, func(int, string) node.NodeList {
		return node.NodeList{nodeA, nodeB}
	})

	var lastUpdate map[string]any
	patches.ApplyFunc(getTemplateImageJobRecordByID, func(ctx context.Context, jobID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{
			JobID:       jobID,
			TemplateID:  "tpl-ext4-guard",
			ResumePhase: JobPhaseDistributing,
			ArtifactID:  "artifact-stale",
		}, nil
	})
	patches.ApplyFunc(updateTemplateImageJob, func(ctx context.Context, jobID string, values map[string]any) error {
		lastUpdate = values
		return nil
	})
	patches.ApplyFunc(unmarshalTemplateImageJobRequest, func(payload string) (*types.CreateTemplateFromImageReq, error) {
		return &types.CreateTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-1"},
			TemplateID:        "tpl-ext4-guard",
			InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
			WritableLayerSize: "20Gi",
			SourceImageRef:    "docker.io/library/nginx:latest",
			Backend:           "xfs",
		}, nil
	})
	patches.ApplyFunc(ListReplicas, func(ctx context.Context, templateID string) ([]models.TemplateReplica, error) {
		return []models.TemplateReplica{
			{NodeID: "node-a", Status: ReplicaStatusReady},
			{NodeID: "node-b", Status: ReplicaStatusFailed},
		}, nil
	})
	patches.ApplyFunc(resolveRedoTargets, func(instanceType string, req *types.RedoTemplateFromImageReq, replicas []models.TemplateReplica) ([]*node.Node, error) {
		return []*node.Node{nodeB}, nil
	})
	// Downgrade to BUILDING_EXT4 because artifact is not reusable.
	patches.ApplyFunc(rootfsArtifactReusableForRedo, func(context.Context, string) bool { return false })
	patches.ApplyFunc(getRootfsArtifactByID, func(ctx context.Context, artifactID string) (*models.RootfsArtifact, error) {
		return &models.RootfsArtifact{ArtifactID: artifactID, Status: ArtifactStatusFailed}, nil
	})
	patches.ApplyFunc(rootfsArtifactReuseVerdict, func(context.Context, *models.RootfsArtifact) error {
		return nil
	})
	patches.ApplyFunc(prepareRootfsArtifactForRedoBuild, func(context.Context, string) (*models.RootfsArtifact, bool) {
		t.Fatal("prepareRootfsArtifactForRedoBuild must not be called before cluster-wide in-use check")
		return nil, false
	})

	var seenHostIDs []string
	origList := listRedoTemplateSandboxes
	t.Cleanup(func() { listRedoTemplateSandboxes = origList })
	listRedoTemplateSandboxes = func(_ context.Context, req *types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
		if req != nil {
			seenHostIDs = append(seenHostIDs, req.HostID)
		}
		return &types.ListCubeSandboxRes{
			Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)},
			Data: []*types.SandboxBriefData{
				{SandboxID: "sb-a", TemplateID: "tpl-ext4-guard", HostID: "node-a", HostIP: "10.0.0.1"},
			},
		}
	}

	runRedoTemplateImageJob(context.Background(), "job-ext4-guard", &types.RedoTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-redo"},
		TemplateID:        "tpl-ext4-guard",
		DistributionScope: []string{"node-b"},
	}, "http://master.example")

	require.Equal(t, []string{""}, seenHostIDs)
	require.NotNil(t, lastUpdate)
	require.Equal(t, JobStatusFailed, lastUpdate["status"])
	require.Contains(t, lastUpdate["error_message"], ErrTemplateInUse.Error())
}

func TestTemplateInUseSeamsKeepDeleteFailOpenAndRedoFailClosed(t *testing.T) {
	require.Equal(t,
		reflect.ValueOf(sandbox.ListSandbox).Pointer(),
		reflect.ValueOf(listTemplateSandboxes).Pointer(),
		"delete/snapshot path must keep sandbox.ListSandbox",
	)
	require.Equal(t,
		reflect.ValueOf(sandbox.ListSandboxWithFailOnError).Pointer(),
		reflect.ValueOf(listRedoTemplateSandboxes).Pointer(),
		"redo path must use sandbox.ListSandboxWithFailOnError",
	)
	require.Equal(t,
		reflect.ValueOf(isTemplateInUseFailClosed).Pointer(),
		reflect.ValueOf(redoTemplateInUse).Pointer(),
	)

	nodeA := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: true}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(localcache.GetHealthyNodesByInstanceType, func(int, string) node.NodeList {
		return node.NodeList{nodeA}
	})

	origDeleteList := listTemplateSandboxes
	origRedoList := listRedoTemplateSandboxes
	t.Cleanup(func() {
		listTemplateSandboxes = origDeleteList
		listRedoTemplateSandboxes = origRedoList
	})

	listTemplateSandboxes = func(context.Context, *types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
		return &types.ListCubeSandboxRes{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)}}
	}
	listRedoTemplateSandboxes = func(context.Context, *types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
		return &types.ListCubeSandboxRes{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_ReqCubeAPIFailed),
				RetMsg:  "cubelet unreachable",
			},
		}
	}

	inUse, err := isTemplateInUse(context.Background(), "tpl-seam", "cubebox")
	require.NoError(t, err)
	require.False(t, inUse)

	_, err = redoTemplateInUse(context.Background(), "tpl-seam", "cubebox")
	require.Error(t, err)
	require.Contains(t, err.Error(), "cubelet unreachable")
}

func TestIsTemplateInUseOnNodesQueriesPeersInParallel(t *testing.T) {
	nodes := []*node.Node{
		{InsID: "node-1", IP: "10.0.0.1", Healthy: true},
		{InsID: "node-2", IP: "10.0.0.2", Healthy: true},
		{InsID: "node-3", IP: "10.0.0.3", Healthy: true},
	}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(localcache.GetHealthyNodesByInstanceType, func(int, string) node.NodeList {
		return node.NodeList(nodes)
	})

	origRedoList := listRedoTemplateSandboxes
	t.Cleanup(func() { listRedoTemplateSandboxes = origRedoList })

	var mu sync.Mutex
	started := 0
	allStarted := make(chan struct{})
	seenHostIDs := make([]string, 0, len(nodes))
	listRedoTemplateSandboxes = func(ctx context.Context, req *types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
		mu.Lock()
		seenHostIDs = append(seenHostIDs, req.HostID)
		started++
		if started == len(nodes) {
			close(allStarted)
		}
		mu.Unlock()

		// Deadlocks until test timeout if isTemplateInUseOnNodes queries nodes sequentially.
		select {
		case <-allStarted:
		case <-time.After(2 * time.Second):
			return &types.ListCubeSandboxRes{
				Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_ReqCubeAPIFailed), RetMsg: "timed out waiting for parallel peer check"},
			}
		case <-ctx.Done():
			return &types.ListCubeSandboxRes{
				Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_ReqCubeAPIFailed), RetMsg: ctx.Err().Error()},
			}
		}
		return &types.ListCubeSandboxRes{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)}}
	}

	inUse, err := isTemplateInUseOnNodes(context.Background(), "tpl-parallel", "cubebox", nodes)
	require.NoError(t, err)
	require.False(t, inUse)
	require.ElementsMatch(t, []string{"node-1", "node-2", "node-3"}, seenHostIDs)
}

func TestRunRedoTemplateImageJobDistributingChecksInUseBeforeArtifactCleanup(t *testing.T) {
	nodeA := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: true}
	nodeB := &node.Node{InsID: "node-b", IP: "10.0.0.2", Healthy: true}

	artifactData := []byte("artifact-data")
	artifactPath, artifactSHA := writeRootfsArtifactTestFile(t, artifactData)
	generatedReqPayload, _ := json.Marshal(&types.CreateCubeSandboxReq{
		InstanceType: cubeboxv1.InstanceType_cubebox.String(),
		Annotations: map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateID:      "tpl-dist-guard",
			constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
		},
	})

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(localcache.GetHealthyNodesByInstanceType, func(int, string) node.NodeList {
		return node.NodeList{nodeA, nodeB}
	})

	var lastUpdate map[string]any
	patches.ApplyFunc(getTemplateImageJobRecordByID, func(ctx context.Context, jobID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{
			JobID:       jobID,
			TemplateID:  "tpl-dist-guard",
			ResumePhase: JobPhaseDistributing,
			ArtifactID:  "artifact-ready",
		}, nil
	})
	patches.ApplyFunc(updateTemplateImageJob, func(ctx context.Context, jobID string, values map[string]any) error {
		lastUpdate = values
		return nil
	})
	patches.ApplyFunc(unmarshalTemplateImageJobRequest, func(payload string) (*types.CreateTemplateFromImageReq, error) {
		return &types.CreateTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-1"},
			TemplateID:        "tpl-dist-guard",
			InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
			WritableLayerSize: "20Gi",
			SourceImageRef:    "docker.io/library/nginx:latest",
			Backend:           "xfs",
		}, nil
	})
	patches.ApplyFunc(ListReplicas, func(ctx context.Context, templateID string) ([]models.TemplateReplica, error) {
		return []models.TemplateReplica{{NodeID: "node-b", Status: ReplicaStatusFailed}}, nil
	})
	patches.ApplyFunc(resolveRedoTargets, func(instanceType string, req *types.RedoTemplateFromImageReq, replicas []models.TemplateReplica) ([]*node.Node, error) {
		return []*node.Node{nodeB}, nil
	})
	patches.ApplyFunc(getRootfsArtifactByID, func(ctx context.Context, artifactID string) (*models.RootfsArtifact, error) {
		return &models.RootfsArtifact{
			ArtifactID:           artifactID,
			Ext4Path:             artifactPath,
			Ext4SHA256:           artifactSHA,
			Ext4SizeBytes:        int64(len(artifactData)),
			DownloadToken:        "token-1",
			MasterNodeIP:         "http://master.example",
			Status:               ArtifactStatusReady,
			GeneratedRequestJSON: string(generatedReqPayload),
		}, nil
	})
	patches.ApplyFunc(cleanupArtifactOnNodes, func(ctx context.Context, artifactID, instanceType string, targets []*node.Node) error {
		t.Fatal("cleanupArtifactOnNodes must not be called before in-use check")
		return nil
	})
	patches.ApplyFunc(distributeRootfsArtifact, func(ctx context.Context, req *types.CreateTemplateFromImageReq, generatedReq *types.CreateCubeSandboxReq, artifact *models.RootfsArtifact, templateID, jobID string) ([]*node.Node, int32, int32, int32, error) {
		t.Fatal("distributeRootfsArtifact must not be called before in-use check")
		return nil, 0, 0, 0, nil
	})

	var seenHostIDs []string
	origList := listRedoTemplateSandboxes
	t.Cleanup(func() { listRedoTemplateSandboxes = origList })
	listRedoTemplateSandboxes = func(_ context.Context, req *types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
		if req != nil {
			seenHostIDs = append(seenHostIDs, req.HostID)
		}
		return &types.ListCubeSandboxRes{
			Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)},
			Data: []*types.SandboxBriefData{
				{SandboxID: "sb-b", TemplateID: "tpl-dist-guard", HostID: "node-b", HostIP: "10.0.0.2"},
			},
		}
	}

	runRedoTemplateImageJob(context.Background(), "job-dist-guard", &types.RedoTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-redo"},
		TemplateID:        "tpl-dist-guard",
		DistributionScope: []string{"node-b"},
	}, "http://master.example")

	require.Equal(t, []string{"node-b"}, seenHostIDs)
	require.NotNil(t, lastUpdate)
	require.Equal(t, JobStatusFailed, lastUpdate["status"])
	require.Contains(t, lastUpdate["error_message"], ErrTemplateInUse.Error())
}

func TestRedoInUseScopeCovers(t *testing.T) {
	nodeA := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: true, HostFacts: &node.HostFacts{CPUIDHash: "cpu-a", HostKernelRelease: "5.15.0"}}
	nodeB := &node.Node{InsID: "node-b", IP: "10.0.0.2", Healthy: true, HostFacts: &node.HostFacts{CPUIDHash: "cpu-b", HostKernelRelease: "6.6.0"}}
	nodeC := &node.Node{InsID: "node-c", IP: "10.0.0.3", Healthy: true, HostFacts: &node.HostFacts{CPUIDHash: "cpu-b", HostKernelRelease: "6.6.0"}}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(localcache.GetHealthyNodesByInstanceType, func(int, string) node.NodeList {
		return node.NodeList{nodeA, nodeB, nodeC}
	})

	_, scopedA := newRedoInUseScope("tpl-scope", "cubebox", "xfs", false, []*node.Node{nodeA})
	_, scopedAB := newRedoInUseScope("tpl-scope", "cubebox", "xfs", false, []*node.Node{nodeA, nodeB})
	_, scopedCluster := newRedoInUseScope("tpl-scope", "cubebox", "xfs", true, []*node.Node{nodeA})
	_, scopedOtherType := newRedoInUseScope("tpl-scope", "otherbox", "xfs", false, []*node.Node{nodeA})
	_, jfsGroupA := newRedoInUseScope("tpl-scope", "cubebox", "jfs", false, []*node.Node{nodeA})
	_, jfsGroupB := newRedoInUseScope("tpl-scope", "cubebox", "jfs", false, []*node.Node{nodeB})

	tests := []struct {
		name string
		pre  redoInUseScope
		cur  redoInUseScope
		want bool
	}{
		{name: "identical scoped", pre: scopedA, cur: scopedA, want: true},
		{name: "narrowed scoped", pre: scopedAB, cur: scopedA, want: true},
		{name: "widened scoped", pre: scopedA, cur: scopedAB, want: false},
		{name: "cluster-wide covers scoped", pre: scopedCluster, cur: scopedAB, want: true},
		{name: "scoped cannot cover cluster-wide", pre: scopedAB, cur: scopedCluster, want: false},
		{name: "instance type mismatch", pre: scopedCluster, cur: scopedOtherType, want: false},
		{name: "jfs group switch", pre: jfsGroupA, cur: jfsGroupB, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.pre.covers(tt.cur))
		})
	}
}

func TestListSandboxesForTemplateCheckFailsClosedOnCancelledContext(t *testing.T) {
	nodeA := &node.Node{InsID: "node-a", IP: "10.0.0.1", Healthy: true}
	nodeB := &node.Node{InsID: "node-b", IP: "10.0.0.2", Healthy: true}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyFunc(localcache.GetHealthyNodesByInstanceType, func(int, string) node.NodeList {
		return node.NodeList{nodeA, nodeB}
	})

	origRedoList := listRedoTemplateSandboxes
	t.Cleanup(func() { listRedoTemplateSandboxes = origRedoList })
	// Simulates doOneList/dealRspData exiting early on ctx.Done() with Success and empty/truncated Data.
	listRedoTemplateSandboxes = func(_ context.Context, _ *types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
		return &types.ListCubeSandboxRes{
			Ret:  &types.Ret{RetCode: int(errorcode.ErrorCode_Success)},
			Data: nil,
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("single target node", func(t *testing.T) {
		inUse, err := isTemplateInUseOnNodes(ctx, "tpl-ctx", "cubebox", []*node.Node{nodeA})
		require.ErrorIs(t, err, context.Canceled)
		require.False(t, inUse)
	})

	t.Run("multiple target nodes", func(t *testing.T) {
		inUse, err := isTemplateInUseOnNodes(ctx, "tpl-ctx", "cubebox", []*node.Node{nodeA, nodeB})
		require.ErrorIs(t, err, context.Canceled)
		require.False(t, inUse)
	})

	t.Run("cluster-wide fail-closed path", func(t *testing.T) {
		inUse, err := isTemplateInUseFailClosed(ctx, "tpl-ctx", "cubebox")
		require.ErrorIs(t, err, context.Canceled)
		require.False(t, inUse)
	})
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

	artifactData := []byte("artifact-data")
	artifactPath, artifactSHA := writeRootfsArtifactTestFile(t, artifactData)
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
			Ext4Path:             artifactPath,
			Ext4SHA256:           artifactSHA,
			Ext4SizeBytes:        int64(len(artifactData)),
			DownloadToken:        "token-1",
			MasterNodeIP:         "http://master.example",
			Status:               ArtifactStatusReady,
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
	origList := listRedoTemplateSandboxes
	t.Cleanup(func() { listRedoTemplateSandboxes = origList })
	listRedoTemplateSandboxes = func(context.Context, *types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
		return &types.ListCubeSandboxRes{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)}}
	}
	origShared := redoArtifactShared
	t.Cleanup(func() { redoArtifactShared = origShared })
	redoArtifactShared = func(context.Context, string, string) (bool, error) {
		return false, nil
	}

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

func TestRunRedoTemplateImageJobReloadsArtifactAfterBuildLock(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	const (
		artifactID = "artifact-lock-refresh"
		staleToken = "token-a"
		freshToken = "token-b"
	)
	artifactData := []byte("artifact-data")
	artifactPath, artifactSHA := writeRootfsArtifactTestFile(t, artifactData)
	targets := []*node.Node{{InsID: "node-a", IP: "10.0.0.1", Healthy: true}}
	// The row as this redo first observes it: the previous build FAILED, so
	// the reusability gate rejects it and the redo downgrades to the
	// BUILDING_EXT4 branch -- the only path that still reloads the row under
	// artifactBuildLocks (prepareRootfsArtifactForRedoBuild).
	staleArtifact := &models.RootfsArtifact{
		ArtifactID:              artifactID,
		TemplateSpecFingerprint: "fingerprint-1",
		Ext4Path:                artifactPath,
		Ext4SHA256:              artifactSHA,
		Ext4SizeBytes:           int64(len(artifactData)),
		DownloadToken:           staleToken,
		Status:                  ArtifactStatusFailed,
		GeneratedRequestJSON:    `{}`,
		ImageConfigJSON:         `{}`,
		SourceImageDigest:       "sha256:digest",
	}
	// The row a concurrent rebuild commits while this redo waits on the build
	// lock: READY again with a fresh token/sha the redo must pick up.
	freshArtifact := *staleArtifact
	freshArtifact.Status = ArtifactStatusReady
	freshArtifact.DownloadToken = freshToken
	freshArtifact.Ext4SHA256 = strings.Repeat("b", 64)

	buildLock := &sync.Mutex{}
	buildLock.Lock()
	lockHeld := true
	artifactBuildLocks.Delete(artifactID)
	artifactBuildLocks.Store(artifactID, buildLock)
	defer func() {
		if lockHeld {
			buildLock.Unlock()
		}
		artifactBuildLocks.Delete(artifactID)
	}()

	initialRead := make(chan struct{})
	lockedReload := make(chan struct{})
	lookupCalls := 0
	distributedToken := ""
	distributedSHA := ""
	generatedToken := ""
	generatedSHA := ""

	patches.ApplyFunc(getTemplateImageJobRecordByID, func(ctx context.Context, jobID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{
			JobID:       jobID,
			TemplateID:  "tpl-1",
			ResumePhase: JobPhaseDistributing,
			ArtifactID:  artifactID,
		}, nil
	})
	patches.ApplyFunc(updateTemplateImageJob, func(ctx context.Context, jobID string, values map[string]any) error {
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
	patches.ApplyFunc(getRootfsArtifactByID, func(ctx context.Context, gotArtifactID string) (*models.RootfsArtifact, error) {
		lookupCalls++
		switch lookupCalls {
		case 1:
			// Reusability gate (unlocked): previous build failed -> not
			// reusable, so the redo downgrades to BUILDING_EXT4.
			close(initialRead)
			copy := *staleArtifact
			return &copy, nil
		case 2:
			// Foreign-artifact re-check on the downgrade path (unlocked): a
			// plain local failure, not held by another CubeMaster.
			copy := *staleArtifact
			return &copy, nil
		case 3:
			// Locked reload inside prepareRootfsArtifactForRedoBuild. This is
			// the lookup the build lock exists for: it must observe the row
			// the concurrent rebuild committed.
			close(lockedReload)
			copy := freshArtifact
			return &copy, nil
		default:
			return nil, fmt.Errorf("unexpected artifact lookup %d", lookupCalls)
		}
	})
	patches.ApplyFunc(cleanupArtifactOnNodes, func(ctx context.Context, gotArtifactID, instanceType string, targets []*node.Node) error {
		return nil
	})
	origList := listRedoTemplateSandboxes
	t.Cleanup(func() { listRedoTemplateSandboxes = origList })
	listRedoTemplateSandboxes = func(context.Context, *types.ListCubeSandboxReq) *types.ListCubeSandboxRes {
		return &types.ListCubeSandboxRes{Ret: &types.Ret{RetCode: int(errorcode.ErrorCode_Success)}}
	}
	origShared := redoArtifactShared
	t.Cleanup(func() { redoArtifactShared = origShared })
	redoArtifactShared = func(context.Context, string, string) (bool, error) {
		return false, nil
	}
	patches.ApplyFunc(distributeRootfsArtifact, func(ctx context.Context, req *types.CreateTemplateFromImageReq, generatedReq *types.CreateCubeSandboxReq, artifact *models.RootfsArtifact, templateID, jobID string) ([]*node.Node, int32, int32, int32, error) {
		distributedToken = artifact.DownloadToken
		distributedSHA = artifact.Ext4SHA256
		if generatedReq != nil && len(generatedReq.Containers) > 0 && generatedReq.Containers[0].Image != nil {
			generatedToken = generatedReq.Containers[0].Image.Annotations[constants.CubeAnnotationRootfsArtifactToken]
			generatedSHA = generatedReq.Containers[0].Image.Annotations[constants.CubeAnnotationRootfsArtifactSHA256]
		}
		return targets, 1, 1, 0, nil
	})
	patches.ApplyFunc(cleanupTemplateReplicasOnNodes, func(ctx context.Context, templateID string, replicas []models.TemplateReplica, targets []*node.Node, backend string) error {
		return nil
	})
	patches.ApplyFunc(ensureTemplateDefinitionWithOptions, func(ctx context.Context, templateID string, storedReq *types.CreateCubeSandboxReq, instanceType, version string, opts definitionCreateOptions) (bool, error) {
		return true, nil
	})
	patches.ApplyFunc(createTemplateReplicasOnNodes, func(ctx context.Context, templateID string, req *types.CreateCubeSandboxReq, targets []*node.Node, opts replicaRunOptions) ([]ReplicaStatus, error) {
		return []ReplicaStatus{{NodeID: "node-a", Status: ReplicaStatusReady}}, nil
	})
	patches.ApplyFunc(refreshTemplateReplicaSummary, func(ctx context.Context, templateID, jobID string) (string, error) {
		return "", nil
	})
	patches.ApplyFunc(GetTemplateInfo, func(ctx context.Context, templateID string) (*TemplateInfo, error) {
		return &TemplateInfo{TemplateID: templateID, Status: StatusReady}, nil
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		runRedoTemplateImageJob(context.Background(), "job-lock-refresh", &types.RedoTemplateFromImageReq{
			Request:    &types.Request{RequestID: "req-redo"},
			TemplateID: "tpl-1",
		}, "http://master.example")
	}()

	select {
	case <-initialRead:
	case <-time.After(5 * time.Second):
		t.Fatal("redo did not read the stale artifact")
	}
	// The decisive reload must happen under the build lock. The gate and the
	// foreign re-check (lookups 1-2) may run unlocked, but the lookup that
	// decides which artifact version gets distributed (lookup 3, inside
	// prepareRootfsArtifactForRedoBuild) must not fire before the lock.
	select {
	case <-lockedReload:
		t.Fatal("redo reloaded the artifact before acquiring the build lock")
	case <-time.After(50 * time.Millisecond):
	}

	// Simulate the concurrent rebuild committing token B before releasing the
	// build lock. Redo must reload this version after it acquires the lock.
	buildLock.Unlock()
	lockHeld = false

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("redo did not finish after the build lock was released")
	}
	if lookupCalls != 3 {
		t.Fatalf("artifact lookups = %d, want reusability gate + foreign re-check + locked reload", lookupCalls)
	}
	if distributedToken != freshToken || generatedToken != freshToken {
		t.Fatalf("distributed token=%q generated token=%q, want refreshed %q", distributedToken, generatedToken, freshToken)
	}
	if distributedSHA != freshArtifact.Ext4SHA256 || generatedSHA != freshArtifact.Ext4SHA256 {
		t.Fatalf("distributed sha=%q generated sha=%q, want refreshed %q", distributedSHA, generatedSHA, freshArtifact.Ext4SHA256)
	}
}

func TestRunRedoTemplateImageJobFailsOnArtifactReloadError(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	const artifactID = "artifact-reload-error"
	reloadErr := errors.New("database connection reset")
	targets := []*node.Node{{InsID: "node-a", IP: "10.0.0.1", Healthy: true}}
	lookupCalls := 0
	var lastUpdate map[string]any

	patches.ApplyFunc(getTemplateImageJobRecordByID, func(ctx context.Context, jobID string) (*models.TemplateImageJob, error) {
		return &models.TemplateImageJob{
			JobID:       jobID,
			TemplateID:  "tpl-1",
			ResumePhase: JobPhaseDistributing,
			ArtifactID:  artifactID,
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
	patches.ApplyFunc(getRootfsArtifactByID, func(ctx context.Context, gotArtifactID string) (*models.RootfsArtifact, error) {
		lookupCalls++
		if gotArtifactID != artifactID {
			return nil, fmt.Errorf("artifact id = %q, want %q", gotArtifactID, artifactID)
		}
		if lookupCalls == 1 {
			return &models.RootfsArtifact{ArtifactID: artifactID}, nil
		}
		return nil, reloadErr
	})

	runRedoTemplateImageJob(context.Background(), "job-reload-error", &types.RedoTemplateFromImageReq{
		Request:    &types.Request{RequestID: "req-redo"},
		TemplateID: "tpl-1",
	}, "http://master.example")

	// Current lookup ordering: (1) the reusability gate, (2) the
	// foreign-artifact re-check on the downgrade path, (3) the locked reload
	// inside prepareRootfsArtifactForRedoBuild. The injected DB error hits
	// lookups 2-3, so the locked reload fails as well: the redo must fail
	// closed rather than reuse bytes it could not verify.
	if lookupCalls != 3 {
		t.Fatalf("artifact lookups = %d, want reusability gate + foreign re-check + locked reload", lookupCalls)
	}
	if lastUpdate == nil || lastUpdate["status"] != JobStatusFailed || lastUpdate["phase"] != JobPhaseBuildingExt4 {
		t.Fatalf("unexpected redo failure update: %+v", lastUpdate)
	}
	// prepareRootfsArtifactForRedoBuild deliberately collapses the reload
	// error into "not reusable", and with local ext4 builds disabled in
	// CubeMaster the only honest outcome is failing the job so the caller
	// forwards a full rebuild to CubeTemplateCenter.
	if got, _ := lastUpdate["error_message"].(string); !strings.Contains(got, "full rebuild") || !strings.Contains(got, "CubeTemplateCenter") {
		t.Fatalf("unexpected reload failure message: %q", got)
	}
}

func TestTemplateImageSubmit_FailFastNodes(t *testing.T) {
	tests := []struct {
		name          string
		healthyNodes  []*node.Node
		scope         []string
		wantErr       error
		errSubstring  string
		expectJobInDB bool
	}{
		{
			name:          "no healthy nodes returns ErrNoTemplateNodes without orphan job",
			healthyNodes:  nil,
			scope:         nil,
			wantErr:       ErrNoTemplateNodes,
			expectJobInDB: false,
		},
		{
			name: "specified offline node returns target nodes not healthy error",
			healthyNodes: []*node.Node{
				{InsID: "node-online-1", IP: "10.0.0.1", Healthy: true},
			},
			scope:         []string{"offline-node"},
			errSubstring:  "target nodes are not healthy or not found: offline-node",
			expectJobInDB: false,
		},
		{
			name: "multiple non-existent nodes return all missing nodes sorted",
			healthyNodes: []*node.Node{
				{InsID: "node-online-1", IP: "10.0.0.1", Healthy: true},
			},
			scope:         []string{"node-missing-2", "node-missing-1"},
			errSubstring:  "target nodes are not healthy or not found: node-missing-1,node-missing-2",
			expectJobInDB: false,
		},
		{
			name: "partial scope missing (1 healthy, 2 missing) fails fast with missing nodes",
			healthyNodes: []*node.Node{
				{InsID: "node-online-1", IP: "10.0.0.1", Healthy: true},
			},
			scope:         []string{"node-online-1", "node-missing-b", "node-missing-a"},
			errSubstring:  "target nodes are not healthy or not found: node-missing-a,node-missing-b",
			expectJobInDB: false,
		},
		{
			name: "healthy node available passes submission and creates job",
			healthyNodes: []*node.Node{
				{InsID: "node-online-1", IP: "10.0.0.1", Healthy: true},
			},
			scope:         []string{"node-online-1"},
			wantErr:       nil,
			expectJobInDB: true,
		},
		{
			name: "default scope passes submission when healthy nodes exist",
			healthyNodes: []*node.Node{
				{InsID: "node-online-1", IP: "10.0.0.1", Healthy: true},
			},
			scope:         nil,
			wantErr:       nil,
			expectJobInDB: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dbName := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
			db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			require.NoError(t, db.AutoMigrate(&models.TemplateDefinition{}, &models.TemplateImageJob{}))
			oldDB := store.db
			store.db = db
			t.Cleanup(func() { store.db = oldDB })

			patches := gomonkey.NewPatches()
			defer patches.Reset()
			patches.ApplyFunc(healthyTemplateNodes, func(instanceType string) []*node.Node {
				return tt.healthyNodes
			})

			req := &types.CreateTemplateFromImageReq{
				Request:           &types.Request{RequestID: "req-" + strings.ReplaceAll(t.Name(), "/", "-")},
				SourceImageRef:    "docker.io/library/nginx:latest",
				WritableLayerSize: "20Gi",
				DistributionScope: tt.scope,
			}

			job, normalizedReq, submitErr := SubmitTemplateFromImageWithoutBuild(context.Background(), req, "http://127.0.0.1:8089")
			if tt.wantErr != nil {
				require.Error(t, submitErr)
				require.True(t, errors.Is(submitErr, tt.wantErr), "expected error %v, got %v", tt.wantErr, submitErr)
				assert.Nil(t, job)
				assert.Nil(t, normalizedReq)
			} else if tt.errSubstring != "" {
				require.Error(t, submitErr)
				assert.Contains(t, submitErr.Error(), tt.errSubstring)
				assert.Nil(t, job)
				assert.Nil(t, normalizedReq)
			} else {
				require.NoError(t, submitErr)
				require.NotNil(t, job)
				require.NotNil(t, normalizedReq)
			}

			var jobCount int64
			require.NoError(t, db.Model(&models.TemplateImageJob{}).Count(&jobCount).Error)
			if tt.expectJobInDB {
				assert.Equal(t, int64(1), jobCount, "expected job record in DB")
			} else {
				assert.Equal(t, int64(0), jobCount, "expected no orphan job in DB")
			}
		})
	}
}

func TestGenerateTemplateCreateRequestAddsCgroupModeAnnotation(t *testing.T) {
	legacy := "legacy"
	req := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-1"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "template-1",
		WritableLayerSize: "20Gi",
		InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
		NetworkType:       cubeboxv1.NetworkType_tap.String(),
		CgroupMode:        &legacy,
	}
	artifact := &models.RootfsArtifact{
		ArtifactID:              "artifact-1",
		TemplateSpecFingerprint: "fingerprint-1",
		Ext4SHA256:              "sha256-1",
		Ext4SizeBytes:           1024,
		DownloadToken:           "token-1",
	}
	got, err := generateTemplateCreateRequest(context.Background(), req, artifact, DockerImageConfig{}, "http://master.example")
	if err != nil {
		t.Fatalf("generateTemplateCreateRequest failed: %v", err)
	}
	if got.Annotations[constants.CubeAnnotationCgroupMode] != "legacy" {
		t.Fatalf("expected cgroup-mode annotation %q, got %q", "legacy", got.Annotations[constants.CubeAnnotationCgroupMode])
	}
}

func TestNormalizeTemplateImageRequestCgroupMode(t *testing.T) {
	base := func(mode *string) *types.CreateTemplateFromImageReq {
		return &types.CreateTemplateFromImageReq{
			Request:           &types.Request{RequestID: "req-1"},
			SourceImageRef:    "docker.io/library/nginx:latest",
			TemplateID:        "template-1",
			WritableLayerSize: "20Gi",
			InstanceType:      cubeboxv1.InstanceType_cubebox.String(),
			NetworkType:       cubeboxv1.NetworkType_tap.String(),
			CgroupMode:        mode,
		}
	}

	legacy := " legacy " // must be trimmed to the canonical value
	got, err := normalizeTemplateImageRequest(base(&legacy))
	if err != nil {
		t.Fatalf("NormalizeRequest(legacy): %v", err)
	}
	if got.CgroupMode == nil || *got.CgroupMode != "legacy" {
		t.Fatalf("expected trimmed CgroupMode=\"legacy\", got %+v", got.CgroupMode)
	}

	blank := "   "
	got, err = normalizeTemplateImageRequest(base(&blank))
	if err != nil {
		t.Fatalf("NormalizeRequest(blank): %v", err)
	}
	if got.CgroupMode != nil {
		t.Fatalf("expected whitespace-only CgroupMode to normalize to nil, got %q", *got.CgroupMode)
	}

	got, err = normalizeTemplateImageRequest(base(nil))
	if err != nil {
		t.Fatalf("NormalizeRequest(nil): %v", err)
	}
	if got.CgroupMode != nil {
		t.Fatalf("expected nil CgroupMode to stay nil, got %q", *got.CgroupMode)
	}

	for _, bad := range []string{"LEGACY", "v1", "unified", "legac"} {
		v := bad
		if _, err := normalizeTemplateImageRequest(base(&v)); err == nil {
			t.Fatalf("NormalizeRequest(%q): expected error, got nil", bad)
		}
	}
}

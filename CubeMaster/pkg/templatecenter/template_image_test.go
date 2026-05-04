// Copyright (c) 2024 Tencent Inc.
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
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

func withTemplateImageConfig(t *testing.T, cfg *config.Config) {
	t.Helper()
	original := getTemplateImageConfig
	getTemplateImageConfig = func() *config.Config {
		return cfg
	}
	t.Cleanup(func() {
		getTemplateImageConfig = original
	})
}

func TestNormalizeTemplateImageRequestDefaults(t *testing.T) {
	withTemplateImageConfig(t, &config.Config{CubeletConf: &config.CubeletConf{}})

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

func TestNormalizeTemplateImageRequestNormalizesExposedPorts(t *testing.T) {
	withTemplateImageConfig(t, &config.Config{CubeletConf: &config.CubeletConf{
		EnableExposedPort: true,
		ExposedPortList:   []string{"80"},
	}})

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
	withTemplateImageConfig(t, &config.Config{CubeletConf: &config.CubeletConf{EnableExposedPort: true}})

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

func TestNormalizeTemplateImageRequestRejectsTooManyCustomExposedPorts(t *testing.T) {
	withTemplateImageConfig(t, &config.Config{CubeletConf: &config.CubeletConf{
		EnableExposedPort: true,
		ExposedPortList:   []string{"80"},
	}})

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

func TestNormalizeTemplateImageRequestTreatsOnlyCubeletDefaultsAsReserved(t *testing.T) {
	withTemplateImageConfig(t, &config.Config{CubeletConf: &config.CubeletConf{
		EnableExposedPort: true,
		ExposedPortList:   []string{"80"},
	}})

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

func TestBuildTemplateSpecFingerprintUsesDigest(t *testing.T) {
	withTemplateImageConfig(t, &config.Config{CubeletConf: &config.CubeletConf{}})

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
	got, err := generateTemplateCreateRequest(req, artifact, dockerImageConfig{
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
	got, err := generateTemplateCreateRequest(req, artifact, dockerImageConfig{}, "http://master.example")
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

func TestPrepareSourceImageSkipsPullWhenImageExistsLocally(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	inspectCalls := 0
	inspectPayload := `[{"RepoDigests":["docker.io/library/nginx@sha256:abcd"],"Config":{"Env":["A=B"],"WorkingDir":"/workspace"}}]`

	patches.ApplyFunc(dockerOutput, func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		if len(args) == 3 && args[0] == "image" && args[1] == "inspect" && args[2] == "docker.io/library/nginx:latest" {
			inspectCalls++
			return []byte(inspectPayload), nil
		}
		if len(args) == 2 && args[0] == "pull" && args[1] == "docker.io/library/nginx:latest" {
			t.Fatal("expected docker pull to be skipped when image exists locally")
		}
		t.Fatalf("unexpected dockerOutput args=%v", args)
		return nil, nil
	})

	got, err := prepareSourceImage(context.Background(), &types.CreateTemplateFromImageReq{
		SourceImageRef: "docker.io/library/nginx:latest",
	}, "http://master.example")
	if err != nil {
		t.Fatalf("prepareSourceImage failed: %v", err)
	}
	if inspectCalls != 1 {
		t.Fatalf("expected 1 inspect call, got %d", inspectCalls)
	}
	if got == nil || got.digest != "docker.io/library/nginx@sha256:abcd" {
		t.Fatalf("unexpected resolved image: %#v", got)
	}
}

func TestPrepareSourceImagePullsAfterLocalInspectMiss(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	inspectCalls := 0
	pullCalled := false
	inspectPayload := `[{"RepoDigests":["docker.io/library/nginx@sha256:abcd"],"Config":{"Cmd":["nginx"]}}]`

	patches.ApplyFunc(dockerOutput, func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		if len(args) == 3 && args[0] == "image" && args[1] == "inspect" && args[2] == "docker.io/library/nginx:latest" {
			inspectCalls++
			if inspectCalls == 1 {
				return nil, errors.New("No such image")
			}
			return []byte(inspectPayload), nil
		}
		if len(args) == 2 && args[0] == "pull" && args[1] == "docker.io/library/nginx:latest" {
			pullCalled = true
			return nil, nil
		}
		t.Fatalf("unexpected dockerOutput args=%v", args)
		return nil, nil
	})

	got, err := prepareSourceImage(context.Background(), &types.CreateTemplateFromImageReq{
		SourceImageRef: "docker.io/library/nginx:latest",
	}, "http://master.example")
	if err != nil {
		t.Fatalf("prepareSourceImage failed: %v", err)
	}
	if !pullCalled {
		t.Fatal("expected docker pull to run after local inspect miss")
	}
	if inspectCalls != 2 {
		t.Fatalf("expected 2 inspect calls, got %d", inspectCalls)
	}
	if got == nil || got.digest != "docker.io/library/nginx@sha256:abcd" {
		t.Fatalf("unexpected resolved image: %#v", got)
	}
}

func TestPrepareSourceImageReturnsPullErrorAfterInspectMiss(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	inspectCalls := 0
	patches.ApplyFunc(dockerOutput, func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		if len(args) == 3 && args[0] == "image" && args[1] == "inspect" && args[2] == "docker.io/library/nginx:latest" {
			inspectCalls++
			return nil, errors.New("No such image")
		}
		if len(args) == 2 && args[0] == "pull" && args[1] == "docker.io/library/nginx:latest" {
			return nil, errors.New("pull denied")
		}
		t.Fatalf("unexpected dockerOutput args=%v", args)
		return nil, nil
	})

	got, err := prepareSourceImage(context.Background(), &types.CreateTemplateFromImageReq{
		SourceImageRef: "docker.io/library/nginx:latest",
	}, "http://master.example")
	if err == nil || !strings.Contains(err.Error(), "docker pull docker.io/library/nginx:latest failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil resolved image on error, got %#v", got)
	}
	if inspectCalls != 1 {
		t.Fatalf("expected 1 inspect call before pull failure, got %d", inspectCalls)
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
	if gotA, gotB := buildCommitTemplateSpecFingerprint(reqA), buildCommitTemplateSpecFingerprint(reqB); gotA != gotB {
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

func TestCleanupIntermediateArtifactsRemovesIntermediateFiles(t *testing.T) {
	workDir := t.TempDir()
	storeDir := filepath.Join(t.TempDir(), "artifact-1")
	storeRootfsDir := filepath.Join(storeDir, "rootfs")
	ext4Path := filepath.Join(storeDir, "artifact-1.ext4")
	if err := os.MkdirAll(filepath.Join(workDir, "rootfs", "etc"), 0o755); err != nil {
		t.Fatalf("MkdirAll work rootfsDir failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(storeRootfsDir, "etc"), 0o755); err != nil {
		t.Fatalf("MkdirAll store rootfsDir failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storeRootfsDir, "etc", "hosts"), []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatalf("WriteFile store rootfs content failed: %v", err)
	}
	if err := os.WriteFile(ext4Path, []byte("ext4"), 0o644); err != nil {
		t.Fatalf("WriteFile ext4Path failed: %v", err)
	}

	cleanupIntermediateArtifacts(workDir, storeRootfsDir, storeDir, false)

	if _, err := os.Stat(workDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workDir should be removed on failure, err=%v", err)
	}
	if _, err := os.Stat(storeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("storeDir should be removed on failure, err=%v", err)
	}
}

func TestCleanupIntermediateArtifactsKeepsStoreDirOnSuccess(t *testing.T) {
	workDir := t.TempDir()
	storeDir := filepath.Join(t.TempDir(), "artifact-1")
	storeRootfsDir := filepath.Join(storeDir, "rootfs")
	ext4Path := filepath.Join(storeDir, "artifact-1.ext4")
	if err := os.MkdirAll(filepath.Join(workDir, "rootfs"), 0o755); err != nil {
		t.Fatalf("MkdirAll work rootfsDir failed: %v", err)
	}
	if err := os.MkdirAll(storeRootfsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll store rootfsDir failed: %v", err)
	}
	if err := os.WriteFile(ext4Path, []byte("ext4"), 0o644); err != nil {
		t.Fatalf("WriteFile ext4Path failed: %v", err)
	}

	cleanupIntermediateArtifacts(workDir, storeRootfsDir, storeDir, true)

	if _, err := os.Stat(workDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workDir should be removed on success, err=%v", err)
	}
	if _, err := os.Stat(storeRootfsDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("storeRootfsDir should be removed, err=%v", err)
	}
	if _, err := os.Stat(ext4Path); err != nil {
		t.Fatalf("ext4Path should be kept on success, err=%v", err)
	}
}

func TestArtifactStoreRootDirDefaultAndEnvOverride(t *testing.T) {
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", "")
	if got := artifactStoreRootDir(); got != defaultArtifactStoreDir {
		t.Fatalf("artifactStoreRootDir default=%q, want %q", got, defaultArtifactStoreDir)
	}

	customDir := filepath.Join(t.TempDir(), "artifact-store")
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", customDir)
	if got := artifactStoreRootDir(); got != customDir {
		t.Fatalf("artifactStoreRootDir=%q, want %q", got, customDir)
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
	fallbackRoot := artifactFallbackStoreRootDir()
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

func TestResolveArtifactStoreDirFallsBackWhenDefaultUnavailable(t *testing.T) {
	t.Setenv("CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR", "")
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	callCount := 0
	patches.ApplyFunc(os.MkdirAll, func(path string, perm os.FileMode) error {
		callCount++
		if strings.Contains(path, defaultArtifactStoreDir) {
			return errors.New("permission denied")
		}
		return nil
	})

	dir, err := resolveArtifactStoreDir(context.Background(), "artifact-1")
	if err != nil {
		t.Fatalf("resolveArtifactStoreDir failed: %v", err)
	}
	want := filepath.Join(artifactFallbackStoreRootDir(), "artifact-1")
	if dir != want {
		t.Fatalf("resolveArtifactStoreDir=%q, want %q", dir, want)
	}
	if callCount < 2 {
		t.Fatalf("expected fallback path preparation, callCount=%d", callCount)
	}
}

func TestCleanupFailedRootfsArtifactKeepsMetadataOnCleanupFailure(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	distributionErr := errors.New("delete image on node-a failed")
	updateCalled := false
	deleteCalled := false

	patches.ApplyFunc(cleanupDistributedArtifact, func(ctx context.Context, artifactID, instanceType string) error {
		return distributionErr
	})
	patches.ApplyFunc(cleanupLocalRootfsArtifact, func(artifactID, ext4Path string) error {
		return nil
	})
	patches.ApplyFunc(deleteRootfsArtifactRecord, func(ctx context.Context, artifactID string) error {
		deleteCalled = true
		return nil
	})
	patches.ApplyFunc(updateRootfsArtifact, func(ctx context.Context, artifactID string, values map[string]any) error {
		updateCalled = true
		if values["status"] != ArtifactStatusFailed {
			t.Fatalf("unexpected status update: %+v", values)
		}
		lastError, _ := values["last_error"].(string)
		if !strings.Contains(lastError, distributionErr.Error()) {
			t.Fatalf("last_error=%q does not contain cleanup error", lastError)
		}
		return nil
	})

	err := cleanupFailedRootfsArtifact(context.Background(), &models.RootfsArtifact{
		ArtifactID: "artifact-1",
		Ext4Path:   filepath.Join(t.TempDir(), "artifact-1", "artifact-1.ext4"),
	}, cubeboxv1.InstanceType_cubebox.String())
	if !errors.Is(err, distributionErr) {
		t.Fatalf("expected distribution cleanup error, got %v", err)
	}
	if deleteCalled {
		t.Fatal("rootfs artifact record should not be deleted when cleanup fails")
	}
	if !updateCalled {
		t.Fatal("rootfs artifact record should be marked failed when cleanup is incomplete")
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
	patches.ApplyFunc(cleanupArtifactOnNodes, func(ctx context.Context, artifactID string, targets []*node.Node) error {
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
	patches.ApplyFunc(prepareLocalSourceImage, func(ctx context.Context, req *types.CreateTemplateFromImageReq, downloadBaseURL string) (*resolvedSourceImage, error) {
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

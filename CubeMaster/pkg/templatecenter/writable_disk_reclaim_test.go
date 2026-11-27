// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

const writableDiskReclaimAnnotationKeyTest = "cube.master.rootfs.writable_disk_reclaim"

func reclaimTemplateReq(t *testing.T, raw string) *types.CreateTemplateFromImageReq {
	t.Helper()
	req := &types.CreateTemplateFromImageReq{
		Request:           &types.Request{RequestID: "req-wdr"},
		SourceImageRef:    "docker.io/library/nginx:latest",
		TemplateID:        "tpl-wdr",
		WritableLayerSize: "20Gi",
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), req); err != nil {
			t.Fatalf("decode request %s: %v", raw, err)
		}
	}
	return req
}

func reclaimArtifact() *models.RootfsArtifact {
	return &models.RootfsArtifact{
		ArtifactID:              "artifact-wdr",
		TemplateSpecFingerprint: "fingerprint-wdr",
		Ext4SHA256:              "sha256-wdr",
		Ext4SizeBytes:           1024,
		DownloadToken:           "token-wdr",
	}
}

func TestGenerateTemplateCreateRequestWritableDiskReclaimPolicy(t *testing.T) {
	for _, tc := range []struct {
		name        string
		request     string
		wantValue   string
		wantBackend string
		wantErr     bool
		errTokens   []string
	}{
		{
			name:    "omitted keeps policy off",
			request: "",
		},
		{
			name:    "explicit false keeps policy off",
			request: `{"writable_disk_reclaim":false}`,
		},
		{
			name:        "true with xfs is recorded",
			request:     `{"writable_disk_reclaim":true,"backend":"xfs"}`,
			wantValue:   "true",
			wantBackend: "xfs",
		},
		{
			name:      "true with omitted backend keeps the historical empty shape",
			request:   `{"writable_disk_reclaim":true}`,
			wantValue: "true",
		},
		{
			name:        "true with uppercase xfs is normalized and recorded",
			request:     `{"writable_disk_reclaim":true,"backend":"XFS"}`,
			wantValue:   "true",
			wantBackend: "xfs",
		},
		{
			name:        "true with jfs is accepted and recorded",
			request:     `{"writable_disk_reclaim":true,"backend":"jfs"}`,
			wantValue:   "true",
			wantBackend: "jfs",
		},
		{
			name:        "true with uppercase jfs is normalized and recorded",
			request:     `{"writable_disk_reclaim":true,"backend":"JFS"}`,
			wantValue:   "true",
			wantBackend: "jfs",
		},
		{
			name:        "true with the juicefs alias is normalized and recorded",
			request:     `{"writable_disk_reclaim":true,"backend":"JuiceFS"}`,
			wantValue:   "true",
			wantBackend: "jfs",
		},
		{
			name:        "true with s3 is accepted and recorded",
			request:     `{"writable_disk_reclaim":true,"backend":"s3"}`,
			wantValue:   "true",
			wantBackend: "s3",
		},
		{
			name:        "true with uppercase s3 is normalized and recorded",
			request:     `{"writable_disk_reclaim":true,"backend":"S3"}`,
			wantValue:   "true",
			wantBackend: "s3",
		},
		{
			name:      "true with an unknown backend is still rejected",
			request:   `{"writable_disk_reclaim":true,"backend":"ceph"}`,
			wantErr:   true,
			errTokens: []string{"backend", "ceph"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := generateTemplateCreateRequest(context.Background(), reclaimTemplateReq(t, tc.request), reclaimArtifact(), DockerImageConfig{}, "http://master.example")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("generateTemplateCreateRequest succeeded, want unsupported-backend rejection")
				}
				msg := strings.ToLower(err.Error())
				for _, token := range tc.errTokens {
					if !strings.Contains(msg, token) {
						t.Fatalf("error %q must mention %q so operators can act on it", err.Error(), token)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("generateTemplateCreateRequest: %v", err)
			}
			if got.Backend != tc.wantBackend {
				t.Fatalf("backend=%q, want %q", got.Backend, tc.wantBackend)
			}
			gotValue, ok := got.Annotations[writableDiskReclaimAnnotationKeyTest]
			if tc.wantValue == "" {
				if ok {
					t.Fatalf("annotation %s=%q, want absent for disabled policy", writableDiskReclaimAnnotationKeyTest, gotValue)
				}
				return
			}
			if !ok || gotValue != tc.wantValue {
				t.Fatalf("annotation %s=%q (present=%t), want %q", writableDiskReclaimAnnotationKeyTest, gotValue, ok, tc.wantValue)
			}
		})
	}
}

// Writable disk reclaim is a runtime VM policy; it must not invalidate the
// reusable read-only rootfs artifact. Runtime state separation comes from the
// persisted template request, not from a redundant fingerprint input.
func TestTemplateSpecFingerprintIgnoresWritableDiskReclaim(t *testing.T) {
	without := buildTemplateSpecFingerprint(reclaimTemplateReq(t, ""), "digest-1")
	with := buildTemplateSpecFingerprint(reclaimTemplateReq(t, `{"writable_disk_reclaim":true}`), "digest-1")
	if without != with {
		t.Fatalf("fingerprint changed when only writable reclaim changed: %q vs %q", without, with)
	}
}

// The template-owned policy follows the actual backend: an enabled reclaim
// template on the JuiceFS backend is valid and keeps its normalized backend.
func TestNormalizeTemplateImageRequestAcceptsEnabledReclaimOnJFS(t *testing.T) {
	for _, raw := range []string{"jfs", "JFS", "juicefs", "JuiceFS"} {
		t.Run(raw, func(t *testing.T) {
			req, err := normalizeTemplateImageRequest(reclaimTemplateReq(t, `{"writable_disk_reclaim":true,"backend":"`+raw+`"}`))
			if err != nil {
				t.Fatalf("normalizeTemplateImageRequest(%q) with enabled reclaim: %v", raw, err)
			}
			if req.Backend != "jfs" {
				t.Fatalf("backend=%q, want jfs", req.Backend)
			}
			if !req.WritableDiskReclaim {
				t.Fatalf("enabled reclaim policy must survive normalization")
			}
		})
	}
}

// The S3 block backend now carries a real discard path, so an enabled reclaim
// template on it is valid and keeps its normalized backend.
func TestNormalizeTemplateImageRequestAcceptsEnabledReclaimOnS3(t *testing.T) {
	for _, raw := range []string{"s3", "S3"} {
		t.Run(raw, func(t *testing.T) {
			req, err := normalizeTemplateImageRequest(reclaimTemplateReq(t, `{"writable_disk_reclaim":true,"backend":"`+raw+`"}`))
			if err != nil {
				t.Fatalf("normalizeTemplateImageRequest(%q) with enabled reclaim: %v", raw, err)
			}
			if req.Backend != "s3" {
				t.Fatalf("backend=%q, want s3", req.Backend)
			}
			if !req.WritableDiskReclaim {
				t.Fatalf("enabled reclaim policy must survive normalization")
			}
		})
	}
}

// An unknown backend has neither a storage nor a discard path, so an enabled
// reclaim policy on it stays invalid.
func TestNormalizeTemplateImageRequestRejectsEnabledReclaimOnUnknownBackend(t *testing.T) {
	_, err := normalizeTemplateImageRequest(reclaimTemplateReq(t, `{"writable_disk_reclaim":true,"backend":"ceph"}`))
	if err == nil {
		t.Fatalf("normalizeTemplateImageRequest succeeded, want unknown-backend rejection")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "ceph") {
		t.Fatalf("error %q must mention ceph", err.Error())
	}
}

func TestCreateTemplateFromImageReqJSONPersistsWritableDiskReclaim(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		want    bool
		wantSet bool
	}{
		{name: "enabled survives template RequestJSON", raw: `{"writable_disk_reclaim":true}`, want: true, wantSet: true},
		{name: "explicit false stays omitted", raw: `{"writable_disk_reclaim":false}`},
		{name: "legacy request has no policy field", raw: `{"source_image_ref":"docker.io/library/nginx:latest"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := reclaimTemplateReq(t, tc.raw)
			body, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("marshal template request: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("decode template request: %v", err)
			}
			got, ok := decoded["writable_disk_reclaim"]
			if ok != tc.wantSet {
				t.Fatalf("writable_disk_reclaim present=%t, want %t in %s", ok, tc.wantSet, body)
			}
			if tc.wantSet {
				if enabled, _ := got.(bool); enabled != tc.want {
					t.Fatalf("writable_disk_reclaim=%v, want %t", got, tc.want)
				}
			}
		})
	}
}

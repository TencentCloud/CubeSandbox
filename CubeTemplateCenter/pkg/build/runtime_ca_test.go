// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package build

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/cube_egress_ca"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
)

func TestRuntimeCABundleSurvivesBuildAndReuseCallbacks(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	reporter := newTestReporter(t, srv)
	ctx := context.Background()
	source := &image.PreparedSource{ConfigJSON: `{"Env":["USER_VALUE=kept"]}`}
	err := reportArtifactBuilt(ctx, "job", &image.BuildResult{}, "artifact", "fingerprint", source,
		reporter, cube_egress_ca.Result{BundlePath: "/bundle", Fingerprint: "ca"}, "", false, nil, "", log.G(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if got["cube_egress_ca_baked"] != true {
		t.Fatal("idempotent verified bundle not reported as baked")
	}
	stored := got["image_config_json"].(string)
	var cfg templatecenter.DockerImageConfig
	if err := json.Unmarshal([]byte(stored), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.CubeEgressCABundlePath != "/bundle" {
		t.Fatal("build callback dropped bundle")
	}
	err = reportExistingArtifact(ctx, "reuse", &models.RootfsArtifact{ArtifactID: "artifact", ImageConfigJSON: stored},
		"fingerprint", source, reporter, []byte("ca"), "ca", false, nil, log.G(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if got["image_config_json"] != stored {
		t.Fatal("reuse callback lost build metadata")
	}
	if source.ConfigJSON != `{"Env":["USER_VALUE=kept"]}` {
		t.Fatal("reuse mutated registry source")
	}
}

func TestRuntimeCABundleBuildReportMetadata(t *testing.T) {
	for _, source := range []string{`{"Env":["A=1"],"custom":42}`, `{"config":{"Env":["A=1"]},"custom":42}`} {
		payload, err := imageConfigWithCABundle(source, "/etc/pki/tls/certs/ca-bundle.crt")
		if err != nil {
			t.Fatal(err)
		}
		var metadata map[string]json.RawMessage
		if err := json.Unmarshal([]byte(payload), &metadata); err != nil {
			t.Fatal(err)
		}
		if string(metadata["custom"]) != "42" {
			t.Fatal("unrelated image metadata lost")
		}
		var cfg templatecenter.DockerImageConfig
		if err := json.Unmarshal([]byte(payload), &cfg); err != nil {
			t.Fatal(err)
		}
		if cfg.CubeEgressCABundlePath != "/etc/pki/tls/certs/ca-bundle.crt" {
			t.Fatalf("path lost: %+v", cfg)
		}
		// A reuse report without a new bake must retain existing metadata.
		reused, err := imageConfigWithCABundle(payload, "")
		if err != nil || reused != payload {
			t.Fatalf("reuse changed metadata: %s %v", reused, err)
		}
	}
	if _, err := imageConfigWithCABundle(`null`, "/bundle"); err == nil {
		t.Fatal("accepted null image config")
	}
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestRuntimeCAEnvironmentPrecedenceAndOptOut(t *testing.T) {
	no := false
	for _, tc := range []struct {
		name        string
		ca, env     *bool
		baked       bool
		path        string
		wantDefault bool
	}{
		{"defaults", nil, nil, true, "/etc/ssl/certs/ca-certificates.crt", true},
		{"RHEL", nil, nil, true, "/etc/pki/tls/certs/ca-bundle.crt", true},
		{"bake disabled", &no, nil, true, "/bundle", false},
		{"env disabled", nil, &no, true, "/bundle", false},
		{"unbaked", nil, nil, false, "/bundle", false},
		{"legacy artifact", nil, nil, true, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &types.CreateTemplateFromImageReq{
				Request: &types.Request{RequestID: "ca-env"}, TemplateID: "ca-template",
				WritableLayerSize: "1Gi", InstanceType: "cubebox", NetworkType: "tap",
				WithCubeCA: tc.ca, WithCubeCAEnv: tc.env,
				ContainerOverrides: &types.ContainerOverrides{Envs: []*types.KeyValue{
					{Key: "REQUESTS_CA_BUNDLE", Value: "/explicit/template.pem"},
					{Key: "CURL_CA_BUNDLE", Value: ""},
				}},
			}
			artifact := &models.RootfsArtifact{ArtifactID: "ca-artifact", CubeEgressCABaked: tc.baked}
			cfg := DockerImageConfig{CubeEgressCABundlePath: tc.path, Env: []string{
				"PATH=/usr/bin", "NODE_EXTRA_CA_CERTS=/explicit/image.pem", "REQUESTS_CA_BUNDLE=/image.pem",
			}}
			got, err := generateTemplateCreateRequest(context.Background(), req, artifact, cfg, "http://master.example")
			if err != nil {
				t.Fatal(err)
			}
			env := map[string]string{}
			for _, kv := range got.Containers[0].Envs {
				env[kv.Key] = kv.Value
			}
			if env["PATH"] != "/usr/bin" || env["NODE_EXTRA_CA_CERTS"] != "/explicit/image.pem" || env["REQUESTS_CA_BUNDLE"] != "/explicit/template.pem" {
				t.Fatalf("explicit values lost: %v", env)
			}
			if v, ok := env["CURL_CA_BUNDLE"]; !ok || v != "" {
				t.Fatalf("explicit empty value lost: %v", env)
			}
			v, ok := env["SSL_CERT_FILE"]
			if ok != tc.wantDefault || (ok && v != tc.path) {
				t.Fatalf("unexpected default: %v", env)
			}
			raw, hasDefaults := got.Annotations[constants.CubeAnnotationRuntimeCAEnv]
			if hasDefaults != tc.wantDefault {
				t.Fatalf("unexpected envd defaults: %s", raw)
			}
			if hasDefaults {
				var defaults map[string]string
				if err := json.Unmarshal([]byte(raw), &defaults); err != nil {
					t.Fatal(err)
				}
				if len(defaults) != 5 {
					t.Fatalf("envd must receive only CA defaults: %v", defaults)
				}
				for key, value := range defaults {
					if value != env[key] {
						t.Fatalf("envd and application differ for %s", key)
					}
				}
			}
		})
	}
}

func TestRuntimeCAConfiguredNamesSurviveJobAndReachApplicationAndEnvd(t *testing.T) {
	for _, names := range [][]string{nil, {}, {"CUSTOM_CA_FILE", "CARGO_HTTP_CAINFO", "CUSTOM_CA_FILE"}} {
		req := &types.CreateTemplateFromImageReq{
			Request: &types.Request{RequestID: "custom-ca"}, SourceImageRef: "python:3.12",
			WritableLayerSize: "1Gi", CubeCAEnvVars: names,
			ContainerOverrides: &types.ContainerOverrides{Envs: []*types.KeyValue{{Key: "CARGO_HTTP_CAINFO", Value: ""}}},
		}
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := unmarshalTemplateImageJobRequest(string(raw))
		if err != nil {
			t.Fatal(err)
		}
		if (stored.CubeCAEnvVars == nil) != (names == nil) {
			t.Fatal("lost omitted versus empty distinction")
		}
		got, err := generateTemplateCreateRequest(context.Background(), stored,
			&models.RootfsArtifact{ArtifactID: "custom-ca", CubeEgressCABaked: true},
			DockerImageConfig{CubeEgressCABundlePath: "/bundle", Env: []string{"CUSTOM_CA_FILE=/image.pem", "OTHER=kept"}}, "http://master.example")
		if err != nil {
			t.Fatal(err)
		}
		app := map[string]string{}
		for _, kv := range got.Containers[0].Envs {
			app[kv.Key] = kv.Value
		}
		if app["CARGO_HTTP_CAINFO"] != "" || app["CUSTOM_CA_FILE"] != "/image.pem" {
			t.Fatal(app)
		}
		envd := map[string]string{}
		if raw := got.Annotations[constants.CubeAnnotationRuntimeCAEnv]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &envd); err != nil {
				t.Fatal(err)
			}
		}
		wanted := runtimeCAEnvNames(stored)
		if len(envd) != len(wanted) {
			t.Fatalf("selected=%v envd=%v", wanted, envd)
		}
		for _, name := range wanted {
			if value, ok := envd[name]; !ok || value != app[name] {
				t.Fatalf("app=%v envd=%v", app, envd)
			}
		}
		if names != nil && len(names) > 0 && !reflect.DeepEqual(stored.CubeCAEnvVars, []string{"CARGO_HTTP_CAINFO", "CUSTOM_CA_FILE"}) {
			t.Fatal(stored.CubeCAEnvVars)
		}
	}
}

func TestRuntimeCAConfiguredNamesValidationAndFingerprint(t *testing.T) {
	for _, name := range []string{"", "A=B", "1BAD", "HAS SPACE", "HAS\nNEWLINE", "变量"} {
		req := &types.CreateTemplateFromImageReq{Request: &types.Request{RequestID: "bad"}, SourceImageRef: "python:3.12", WritableLayerSize: "1Gi", CubeCAEnvVars: []string{name}}
		if _, err := normalizeTemplateImageRequest(req); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	req := &types.CreateTemplateFromImageReq{CubeCAEnvVars: []string{"CUSTOM_CA_FILE", "CARGO_HTTP_CAINFO"}}
	first := buildTemplateSpecFingerprintWithCA(req, "image", "ca")
	req.CubeCAEnvVars = []string{"CARGO_HTTP_CAINFO", "CUSTOM_CA_FILE", "CUSTOM_CA_FILE"}
	if first != buildTemplateSpecFingerprintWithCA(req, "image", "ca") {
		t.Fatal("order or duplicates changed fingerprint")
	}
	req.CubeCAEnvVars = []string{"NEW_CA_FILE"}
	if first == buildTemplateSpecFingerprintWithCA(req, "image", "ca") {
		t.Fatal("changed selection reused template spec")
	}
	no := false
	req.WithCubeCAEnv = &no
	before := buildTemplateSpecFingerprintWithCA(req, "image", "ca")
	req.CubeCAEnvVars = nil
	if before != buildTemplateSpecFingerprintWithCA(req, "image", "ca") {
		t.Fatal("disabled selection affected fingerprint")
	}
	req.WithCubeCAEnv = nil
	before = buildTemplateSpecFingerprintWithCA(req, "image", "")
	req.CubeCAEnvVars = []string{"NEW_CA_FILE"}
	if before != buildTemplateSpecFingerprintWithCA(req, "image", "") {
		t.Fatal("unbaked selection affected fingerprint")
	}
}

func TestRuntimeCAMetadataSurvivesImageConfigDecodeAndPersistence(t *testing.T) {
	for _, payload := range []string{
		`{"Env":["A=1"],"_cube_egress_ca_bundle_path":"/bundle"}`,
		`{"config":{"Env":["A=1"]},"_cube_egress_ca_bundle_path":"/bundle"}`,
		`{"Config":{"Env":["A=1"]},"_cube_egress_ca_bundle_path":"/bundle"}`,
	} {
		cfg, err := decodeImageConfigJSON(payload)
		if err != nil || cfg.CubeEgressCABundlePath != "/bundle" || len(cfg.Env) != 1 {
			t.Fatalf("decoded=%+v err=%v", cfg, err)
		}
		stored, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		reused, err := decodeImageConfigJSON(string(stored))
		if err != nil || reused.CubeEgressCABundlePath != "/bundle" {
			t.Fatalf("reuse lost metadata: %+v %v", reused, err)
		}
	}
}

func TestRuntimeCAFingerprintSeparatesLegacyAndOptOut(t *testing.T) {
	req := &types.CreateTemplateFromImageReq{SourceImageRef: "image", WritableLayerSize: "1Gi"}
	withEnv := buildTemplateSpecFingerprintWithCA(req, "digest", "ca")
	no := false
	req.WithCubeCAEnv = &no
	withoutEnv := buildTemplateSpecFingerprintWithCA(req, "digest", "ca")
	if withEnv == withoutEnv {
		t.Fatal("new runtime defaults could reuse a legacy artifact")
	}
	withoutCA := buildTemplateSpecFingerprintWithCA(req, "digest", "")
	req.WithCubeCAEnv = nil
	if got := buildTemplateSpecFingerprintWithCA(req, "digest", ""); got != withoutCA {
		t.Fatal("env option should not affect artifacts when CA baking is disabled")
	}
}

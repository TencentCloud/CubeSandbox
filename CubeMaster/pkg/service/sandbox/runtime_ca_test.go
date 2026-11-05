// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"encoding/json"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestRuntimeCADefaultsReachEnvdInitWithoutReplacingExplicitEnv(t *testing.T) {
	for _, capability := range []string{"injected", "preinstalled", "none", "legacy"} {
		t.Run(capability, func(t *testing.T) {
			req := &types.CreateCubeSandboxReq{
				Annotations:       map[string]string{constants.CubeAnnotationRuntimeCAEnv: `{"NODE_EXTRA_CA_CERTS":"/bundle","SSL_CERT_FILE":"/bundle","REQUESTS_CA_BUNDLE":"/image.pem","CURL_CA_BUNDLE":"","CUSTOM_CA_FILE":"/bundle","CARGO_HTTP_CAINFO":"/bundle"}`},
				CreateTimeEnvVars: map[string]string{"NODE_EXTRA_CA_CERTS": "", "REQUESTS_CA_BUNDLE": "/sdk.pem", "SESSION": "42"},
			}
			switch capability {
			case "injected":
				req.Annotations[constants.CubeAnnotationsInjectEnvd] = "true"
			case "preinstalled":
				req.Annotations[constants.CubeAnnotationComponentEnvdVersion] = "0.5.11"
			case "legacy":
				req.Annotations[constants.CubeAnnotationComponentEnvdVersion] = "0.5.11"
				delete(req.Annotations, constants.CubeAnnotationRuntimeCAEnv)
			}
			envs, err := envdInitEnvVars(req)
			if err != nil {
				t.Fatal(err)
			}
			out := map[string]string{}
			if err := setCreateTimeEnvVarsAnnotation(out, envs); err != nil {
				t.Fatal(err)
			}
			var got map[string]string
			if err := json.Unmarshal([]byte(out[constants.CubeAnnotationCreateTimeEnvVars]), &got); err != nil {
				t.Fatal(err)
			}
			if got["NODE_EXTRA_CA_CERTS"] != "" || got["REQUESTS_CA_BUNDLE"] != "/sdk.pem" || got["SESSION"] != "42" {
				t.Fatalf("explicit create values lost: %v", got)
			}
			wantDefaults := capability == "injected" || capability == "preinstalled"
			for _, key := range []string{"CUSTOM_CA_FILE", "CARGO_HTTP_CAINFO"} {
				if value, ok := got[key]; ok != wantDefaults || (ok && value != "/bundle") {
					t.Fatalf("custom default lost: %v", got)
				}
			}
			if value, ok := got["SSL_CERT_FILE"]; ok != wantDefaults || (ok && value != "/bundle") {
				t.Fatalf("unexpected CA default: %v", got)
			}
			if len(req.CreateTimeEnvVars) != 3 {
				t.Fatal("mutated caller environment")
			}
		})
	}
}

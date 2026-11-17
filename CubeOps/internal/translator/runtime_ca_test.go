// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package translator

import (
	"reflect"
	"testing"
)

func TestTransformCreateTemplateRequestCustomCAEnvNames(t *testing.T) {
	for _, value := range []interface{}{nil, []interface{}{}, []interface{}{"CARGO_HTTP_CAINFO", "CUSTOM_CA_FILE"}} {
		got := TransformCreateTemplateRequest(map[string]interface{}{"image": "python:3.12", "cube_ca_env_vars": value})
		if actual, ok := got["cube_ca_env_vars"]; !ok || !reflect.DeepEqual(actual, value) {
			t.Fatalf("lost selection: %v", got)
		}
	}
	got := TransformCreateTemplateRequest(map[string]interface{}{"image": "python:3.12"})
	if _, ok := got["cube_ca_env_vars"]; ok {
		t.Fatal("omitted selection became explicit")
	}
}

func TestTransformCreateTemplateRequestRuntimeCAOptions(t *testing.T) {
	for _, key := range []string{"with_cube_ca", "with_cube_ca_env"} {
		t.Run(key, func(t *testing.T) {
			for _, value := range []interface{}{true, false, nil} {
				body := map[string]interface{}{"image": "python:3.11-slim", key: value}
				got := TransformCreateTemplateRequest(body)
				if actual, present := got[key]; !present || actual != value {
					t.Fatalf("%s=%v: translated value=%v present=%v", key, value, actual, present)
				}
			}
			omitted := TransformCreateTemplateRequest(map[string]interface{}{"image": "python:3.11-slim"})
			if _, present := omitted[key]; present {
				t.Fatalf("omitted %s must retain the server default", key)
			}
		})
	}
	got := TransformCreateTemplateRequest(map[string]interface{}{
		"image": "python:3.11-slim", "with_cube_ca": true, "with_cube_ca_env": false,
	})
	if got["with_cube_ca"] != true || got["with_cube_ca_env"] != false {
		t.Fatalf("runtime opt-out must not disable baking: %v", got)
	}
}

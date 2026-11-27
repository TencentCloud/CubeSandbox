// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"strings"
	"testing"

	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func assertWritableDiskReclaimPolicy(t *testing.T, err error, annotations map[string]string, want string, allowReject bool) {
	t.Helper()
	if err != nil {
		if !allowReject || !strings.Contains(strings.ToLower(err.Error()), "writable") {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	got := annotations[writableDiskReclaimAnnotationKeyTest]
	if got != want {
		t.Fatalf("annotation %s=%q (present=%t), want %q", writableDiskReclaimAnnotationKeyTest, got, got != "", want)
	}
}

// applyTemplateRequest is the tail of CubeMaster's ResolveTemplate path. The
// persisted template policy owns the value: a caller annotation may neither
// enable a legacy template nor disable a reclaim-enabled one.
func TestApplyTemplateRequestWritableDiskReclaimFollowsTemplatePolicy(t *testing.T) {
	for _, tc := range []struct {
		name            string
		templateValue   string
		templatePresent bool
		requestValue    string
		requestPresent  bool
		want            string
		allowReject     bool
	}{
		{
			name:          "template enables, request omits",
			templateValue: "true", templatePresent: true,
			want: "true",
		},
		{
			name:          "template enables, request matches",
			templateValue: "true", templatePresent: true,
			requestValue: "true", requestPresent: true,
			want: "true",
		},
		{
			name:          "template enables, request tries to disable",
			templateValue: "true", templatePresent: true,
			requestValue: "false", requestPresent: true,
			want: "true", allowReject: true,
		},
		{
			name: "legacy template missing policy stays off",
			want: "",
		},
		{
			name:         "legacy template missing policy rejects forged enable",
			requestValue: "true", requestPresent: true,
			want: "", allowReject: true,
		},
		{
			name:          "template stores false, request forge is rejected",
			templateValue: "false", templatePresent: true,
			requestValue: "true", requestPresent: true,
			want: "", allowReject: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			templateReq := &sandboxtypes.CreateCubeSandboxReq{}
			if tc.templatePresent {
				templateReq.Annotations = map[string]string{writableDiskReclaimAnnotationKeyTest: tc.templateValue}
			}
			req := &sandboxtypes.CreateCubeSandboxReq{}
			if tc.requestPresent {
				req.Annotations = map[string]string{writableDiskReclaimAnnotationKeyTest: tc.requestValue}
			}

			err := applyTemplateRequest(templateReq, req)
			assertWritableDiskReclaimPolicy(t, err, req.Annotations, tc.want, tc.allowReject)
		})
	}
}

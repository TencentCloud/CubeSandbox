// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestParseTemplateUser(t *testing.T) {
	tests := []struct {
		spec     string
		username string
		uid      *int64
		gid      *int64
	}{
		{spec: "1000", uid: int64ptr(1000)},
		{spec: "0:0", uid: int64ptr(0), gid: int64ptr(0)},
		{spec: "1000:1000", uid: int64ptr(1000), gid: int64ptr(1000)},
		{spec: "sandbox", username: "sandbox"},
		{spec: "sandbox:1000", username: "sandbox", gid: int64ptr(1000)},
		{spec: "  _apt  ", username: "_apt"},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			got, err := ParseTemplateUser(tt.spec)
			if err != nil {
				t.Fatalf("ParseTemplateUser: %v", err)
			}
			if !got.Privileged {
				t.Fatal("expected privileged to stay true")
			}
			if got.ReadonlyRootfs {
				t.Fatal("expected writable rootfs")
			}
			if got.RunAsUsername != tt.username {
				t.Fatalf("username=%q, want %q", got.RunAsUsername, tt.username)
			}
			assertInt64Value(t, "uid", got.RunAsUser, tt.uid)
			assertInt64Value(t, "gid", got.RunAsGroup, tt.gid)
		})
	}
}

func TestParseTemplateUserRejects(t *testing.T) {
	for _, spec := range []string{"", "   ", ":1000", "1000:", "a:b:c", "1000:sandbox", "-1", "1user", "has space", "user/name"} {
		t.Run(spec, func(t *testing.T) {
			if _, err := ParseTemplateUser(spec); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestTryParseImageUserDropsNamedGroup(t *testing.T) {
	if got := tryParseImageUser("nobody:nogroup"); got != nil {
		t.Fatalf("named group should be ignored, got %+v", got)
	}
	got := tryParseImageUser("nobody")
	if got == nil || got.RunAsUsername != "nobody" || !got.Privileged {
		t.Fatalf("image username was not applied: %+v", got)
	}
}

func TestValidateTemplateContainerUser(t *testing.T) {
	err := validateTemplateContainerUser(&types.ContainerOverrides{
		SecurityContext: &types.ContainerSecurityContext{
			RunAsGroup: &types.Int64Value{Value: 1000},
		},
	})
	if err == nil {
		t.Fatal("expected group without user to fail")
	}
	err = validateTemplateContainerUser(&types.ContainerOverrides{
		SecurityContext: &types.ContainerSecurityContext{
			RunAsUser: &types.Int64Value{Value: -1},
		},
	})
	if err == nil {
		t.Fatal("expected negative uid to fail")
	}
}

func int64ptr(v int64) *int64 { return &v }

func assertInt64Value(t *testing.T, name string, got *types.Int64Value, want *int64) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("%s=%d, want unset", name, got.Value)
		}
		return
	}
	if got == nil || got.Value != *want {
		t.Fatalf("%s=%v, want %d", name, got, *want)
	}
}

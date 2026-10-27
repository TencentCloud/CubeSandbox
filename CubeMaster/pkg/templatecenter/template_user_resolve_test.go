// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

const testPasswd = `root:x:0:0:root:/root:/bin/bash
# comment line
broken-line
sandbox:x:1000:1001:Sandbox:/home/sandbox:/bin/sh
sandbox:x:2000:2000:dup:/tmp:/bin/sh
nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin
`

func writeTestRootfs(t *testing.T, passwd string) string {
	t.Helper()
	dir := t.TempDir()
	if passwd != "" {
		if err := os.MkdirAll(filepath.Join(dir, "etc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "etc", "passwd"), []byte(passwd), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func userReq(t *testing.T, spec string) *types.CreateTemplateFromImageReq {
	t.Helper()
	sc, err := ParseTemplateUser(spec)
	if err != nil {
		t.Fatalf("ParseTemplateUser(%q): %v", spec, err)
	}
	return &types.CreateTemplateFromImageReq{ContainerOverrides: &types.ContainerOverrides{SecurityContext: sc}}
}

func TestResolveTemplateUserFromRootfs(t *testing.T) {
	rootfs := writeTestRootfs(t, testPasswd)
	tests := []struct {
		spec string
		want ResolvedTemplateUser
	}{
		{"sandbox", ResolvedTemplateUser{UID: 1000, GID: 1001, InPasswd: true}},
		{"sandbox:5", ResolvedTemplateUser{UID: 1000, GID: 5, InPasswd: true}},
		{"1000", ResolvedTemplateUser{UID: 1000, GID: 1001, InPasswd: true}},
		{"1000:1000", ResolvedTemplateUser{UID: 1000, GID: 1000, InPasswd: true}},
		{"0", ResolvedTemplateUser{UID: 0, GID: 0, InPasswd: true}},
		{"4242", ResolvedTemplateUser{UID: 4242, GID: 0, InPasswd: false}},
		{"4242:7", ResolvedTemplateUser{UID: 4242, GID: 7, InPasswd: false}},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			got, err := ResolveTemplateUserFromRootfs(rootfs, userReq(t, tt.spec))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil || *got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResolveTemplateUserUnknownUsername(t *testing.T) {
	rootfs := writeTestRootfs(t, testPasswd)
	_, err := ResolveTemplateUserFromRootfs(rootfs, userReq(t, "ghost"))
	if !errors.Is(err, ErrTemplateUserNotFound) || !strings.Contains(err.Error(), `"ghost"`) {
		t.Fatalf("want ErrTemplateUserNotFound for ghost, got %v", err)
	}
}

func TestResolveTemplateUserWithoutPasswd(t *testing.T) {
	rootfs := writeTestRootfs(t, "")
	if _, err := ResolveTemplateUserFromRootfs(rootfs, userReq(t, "sandbox")); !errors.Is(err, ErrTemplateUserNotFound) {
		t.Fatalf("username without /etc/passwd must fail, got %v", err)
	}
	got, err := ResolveTemplateUserFromRootfs(rootfs, userReq(t, "1000"))
	if err != nil || got == nil || *got != (ResolvedTemplateUser{UID: 1000, GID: 0}) {
		t.Fatalf("numeric uid without /etc/passwd must pass with gid 0, got %+v %v", got, err)
	}
}

func TestResolveTemplateUserRejectsPasswdSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "passwd"), []byte("evil:x:7:7::/:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "passwd"), filepath.Join(rootfs, "etc", "passwd")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveTemplateUserFromRootfs(rootfs, userReq(t, "evil")); !errors.Is(err, ErrTemplateUserNotFound) {
		t.Fatalf("symlink escaping the rootfs must not be followed, got %v", err)
	}
}

func TestResolveTemplateUserNoUserIsNoop(t *testing.T) {
	rootfs := writeTestRootfs(t, "")
	for _, req := range []*types.CreateTemplateFromImageReq{
		nil,
		{},
		{ContainerOverrides: &types.ContainerOverrides{}},
		{ContainerOverrides: &types.ContainerOverrides{SecurityContext: &types.ContainerSecurityContext{Privileged: true}}},
	} {
		got, err := ResolveTemplateUserFromRootfs(rootfs, req)
		if got != nil || err != nil {
			t.Fatalf("request without user must be a no-op, got %+v %v", got, err)
		}
		if out := withResolvedTemplateUser(req, &ResolvedTemplateUser{UID: 1, GID: 1}); out != req {
			t.Fatalf("request without user must be returned unchanged")
		}
	}
}

func TestWithResolvedTemplateUserSendsNumbersOnly(t *testing.T) {
	req := userReq(t, "sandbox")
	out := withResolvedTemplateUser(req, &ResolvedTemplateUser{UID: 1000, GID: 1001})
	sc := out.ContainerOverrides.SecurityContext
	if sc.RunAsUsername != "" || sc.RunAsUser.Value != 1000 || sc.RunAsGroup.Value != 1001 || !sc.Privileged {
		t.Fatalf("unexpected security context %+v", sc)
	}
	if req.ContainerOverrides.SecurityContext.RunAsUsername != "sandbox" {
		t.Fatalf("input request must not be mutated")
	}
	if withResolvedTemplateUser(req, nil) != req {
		t.Fatalf("nil resolution (old TC) must keep the request unchanged")
	}
}

func TestResolvedTemplateUserRoundTrip(t *testing.T) {
	sc := &types.ContainerSecurityContext{Privileged: true, RunAsUser: &types.Int64Value{Value: 1000}, RunAsGroup: &types.Int64Value{Value: 1001}}
	raw, _ := json.Marshal(types.CreateCubeSandboxReq{Containers: []*types.Container{{SecurityContext: sc}}})
	got := resolvedTemplateUserFromArtifact(&models.RootfsArtifact{GeneratedRequestJSON: string(raw)})
	if got == nil || got.UID != 1000 || got.GID != 1001 {
		t.Fatalf("got %+v", got)
	}
	// Legacy artifacts that still carry a username are not treated as resolved.
	sc.RunAsUsername = "sandbox"
	raw, _ = json.Marshal(types.CreateCubeSandboxReq{Containers: []*types.Container{{SecurityContext: sc}}})
	if got := resolvedTemplateUserFromArtifact(&models.RootfsArtifact{GeneratedRequestJSON: string(raw)}); got != nil {
		t.Fatalf("username artifact must not resolve, got %+v", got)
	}

	var payload map[string]any
	b, _ := json.Marshal(map[string]any{"resolved_user": &ResolvedTemplateUser{UID: 1000, GID: 1001, InPasswd: true}})
	_ = json.Unmarshal(b, &payload)
	if d := DecodeResolvedTemplateUser(payload["resolved_user"]); d == nil || !reflect.DeepEqual(*d, ResolvedTemplateUser{UID: 1000, GID: 1001, InPasswd: true}) {
		t.Fatalf("decode got %+v", d)
	}
	if DecodeResolvedTemplateUser(nil) != nil || DecodeResolvedTemplateUser(map[string]any{"uid": -1, "gid": 0}) != nil {
		t.Fatalf("missing or invalid resolved_user must decode to nil")
	}
}

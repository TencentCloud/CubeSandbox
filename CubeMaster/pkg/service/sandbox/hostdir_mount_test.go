// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestInjectHostDirMounts_AllowedPrefix(t *testing.T) {
	tests := []struct {
		name    string
		opts    []HostDirMountOption
		wantErr bool
	}{
		{
			name: "valid path under /data/shared",
			opts: []HostDirMountOption{
				{HostPath: "/data/shared/mydir", MountPath: "/mnt/data"},
			},
			wantErr: false,
		},
		{
			name: "valid nested path",
			opts: []HostDirMountOption{
				{HostPath: "/data/shared/team/project/files", MountPath: "/workspace"},
			},
			wantErr: false,
		},
		{
			name: "rejected - root path",
			opts: []HostDirMountOption{
				{HostPath: "/", MountPath: "/mnt"},
			},
			wantErr: true,
		},
		{
			name: "rejected - /etc",
			opts: []HostDirMountOption{
				{HostPath: "/etc/passwd", MountPath: "/mnt/passwd"},
			},
			wantErr: true,
		},
		{
			name: "rejected - path traversal attempt",
			opts: []HostDirMountOption{
				{HostPath: "/data/shared/../etc/shadow", MountPath: "/mnt/shadow"},
			},
			wantErr: true,
		},
		{
			name: "rejected - similar prefix but not under /data/shared/",
			opts: []HostDirMountOption{
				{HostPath: "/data/shared_evil", MountPath: "/mnt"},
			},
			wantErr: true,
		},
		{
			name: "allowed - exact /data/shared directory",
			opts: []HostDirMountOption{
				{HostPath: "/data/shared", MountPath: "/mnt"},
			},
			wantErr: false,
		},
		{
			name: "rejected - relative host path",
			opts: []HostDirMountOption{
				{HostPath: "data/shared/foo", MountPath: "/mnt"},
			},
			wantErr: true,
		},
		{
			name: "rejected - relative mount path",
			opts: []HostDirMountOption{
				{HostPath: "/data/shared/foo", MountPath: "mnt"},
			},
			wantErr: true,
		},
		{
			name: "mixed valid and invalid entries",
			opts: []HostDirMountOption{
				{HostPath: "/data/shared/ok", MountPath: "/mnt/ok"},
				{HostPath: "/etc/secret", MountPath: "/mnt/secret"},
			},
			wantErr: true,
		},
		{
			name: "path with redundant dots gets cleaned",
			opts: []HostDirMountOption{
				{HostPath: "/data/shared/foo/../bar", MountPath: "/mnt/bar"},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			req := &types.CreateCubeSandboxReq{
				Annotations: map[string]string{
					AnnotationHostDirMount: string(raw),
				},
			}
			err = injectHostDirMounts(context.Background(), req)
			if (err != nil) != tt.wantErr {
				t.Errorf("injectHostDirMounts() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestInjectHostDirMounts_MalformedJSON(t *testing.T) {
	req := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			AnnotationHostDirMount: `not valid json`,
		},
	}
	err := injectHostDirMounts(context.Background(), req)
	if err == nil {
		t.Error("expected error for malformed JSON annotation, got nil")
	}
}

func TestInjectPluginVolumeMounts_Readonly(t *testing.T) {
	tests := []struct {
		name         string
		annotation   string
		wantReadonly bool
	}{
		{
			name:         "readonly forwarded",
			annotation:   `[{"name":"dataset-volume","container_path":"/dataset","readonly":true}]`,
			wantReadonly: true,
		},
		{
			name:         "readonly omitted defaults to false",
			annotation:   `[{"name":"workspace-volume","container_path":"/workspace"}]`,
			wantReadonly: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &types.CreateCubeSandboxReq{
				Annotations: map[string]string{
					AnnotationPluginVolumeMounts: tt.annotation,
				},
				Containers: []*types.Container{{Name: "main"}, {Name: "sidecar"}},
			}

			if err := injectPluginVolumeMounts(context.Background(), req); err != nil {
				t.Fatalf("injectPluginVolumeMounts() error = %v", err)
			}
			for _, container := range req.Containers {
				if got := len(container.VolumeMounts); got != 1 {
					t.Fatalf("container %q len(VolumeMounts) = %d, want 1", container.Name, got)
				}
				if got := container.VolumeMounts[0].Readonly; got != tt.wantReadonly {
					t.Errorf("container %q VolumeMounts[0].Readonly = %v, want %v", container.Name, got, tt.wantReadonly)
				}
			}
		})
	}
}

func TestInjectPluginVolumeMounts_InvalidReadonlyType(t *testing.T) {
	req := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			AnnotationPluginVolumeMounts: `[{"name":"dataset","container_path":"/dataset","readonly":"true"}]`,
		},
		Containers: []*types.Container{{Name: "main"}},
	}

	if err := injectPluginVolumeMounts(context.Background(), req); err == nil {
		t.Fatal("injectPluginVolumeMounts() error = nil, want invalid readonly type error")
	}
}

func TestValidateHostPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantErr  bool
		wantPath string
	}{
		{"under allowed prefix", "/data/shared/foo", false, "/data/shared/foo"},
		{"exact allowed dir", "/data/shared", false, "/data/shared"},
		{"traversal escape", "/data/shared/../secret", true, ""},
		{"unrelated path", "/tmp/data", true, ""},
		{"prefix spoof", "/data/shared_hack/x", true, ""},
		{"path with dots cleaned", "/data/shared/a/../b", false, "/data/shared/b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateHostPath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateHostPath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.wantPath {
				t.Errorf("validateHostPath(%q) = %q, want %q", tt.path, got, tt.wantPath)
			}
		})
	}
}

func TestInjectHostDirMountsSetsHostPathOnVolumeMount(t *testing.T) {
	req := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			AnnotationHostDirMount: `[{"hostPath":"/data/shared/data","mountPath":"/mnt/data","readOnly":true}]`,
		},
		Containers: []*types.Container{{Name: "work"}},
	}

	if err := injectHostDirMounts(context.Background(), req); err != nil {
		t.Fatalf("injectHostDirMounts() error=%v", err)
	}
	if len(req.Containers[0].VolumeMounts) != 1 {
		t.Fatalf("volume mount count=%d want 1", len(req.Containers[0].VolumeMounts))
	}
	mount := req.Containers[0].VolumeMounts[0]
	if mount.GetHostPath() != "/data/shared/data" {
		t.Fatalf("HostPath=%q want /data/shared/data", mount.GetHostPath())
	}
	if mount.GetContainerPath() != "/mnt/data" || !mount.GetReadonly() {
		t.Fatalf("unexpected mount: %+v", mount)
	}
}

func TestInjectHostDirMountsIsIdempotentForSnapshotRestore(t *testing.T) {
	req := &types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			AnnotationHostDirMount: `[
				{"hostPath":"/data/shared/rw","mountPath":"/mnt/rw"},
				{"hostPath":"/data/shared/ro","mountPath":"/mnt/ro","readOnly":true}
			]`,
		},
		Containers: []*types.Container{{Name: "work"}},
	}

	if err := injectHostDirMounts(context.Background(), req); err != nil {
		t.Fatalf("first injectHostDirMounts() error=%v", err)
	}
	if err := injectHostDirMounts(context.Background(), req); err != nil {
		t.Fatalf("second injectHostDirMounts() error=%v", err)
	}
	if len(req.Volumes) != 2 {
		t.Fatalf("volume count=%d want 2", len(req.Volumes))
	}
	if len(req.Containers[0].VolumeMounts) != 2 {
		t.Fatalf("volume mount count=%d want 2", len(req.Containers[0].VolumeMounts))
	}
}

func TestCreateRequestHasHostMount(t *testing.T) {
	t.Parallel()
	if createRequestHasHostMount(nil) {
		t.Fatal("nil spec must not pin")
	}
	if createRequestHasHostMount(&types.CreateCubeSandboxReq{}) {
		t.Fatal("empty spec must not pin")
	}
	if createRequestHasHostMount(&types.CreateCubeSandboxReq{
		Annotations: map[string]string{AnnotationHostDirMount: "[]"},
	}) {
		t.Fatal("empty host-mount list must not pin")
	}
	if !createRequestHasHostMount(&types.CreateCubeSandboxReq{
		Annotations: map[string]string{
			AnnotationHostDirMount: `[{"hostPath":"/data/shared/a","mountPath":"/mnt"}]`,
		},
	}) {
		t.Fatal("host-mount annotation must pin")
	}
	if !createRequestHasHostMount(&types.CreateCubeSandboxReq{
		Annotations: map[string]string{AnnotationHostDirMount: `not-json`},
	}) {
		t.Fatal("malformed host-mount annotation must pin")
	}
	if !createRequestHasHostMount(&types.CreateCubeSandboxReq{
		Volumes: []*types.Volume{{
			VolumeSource: &types.VolumeSource{
				HostDirVolumeSources: &types.HostDirVolumeSources{
					VolumeSources: []*types.HostDirSource{{HostPath: "/data/shared/a"}},
				},
			},
		}},
	}) {
		t.Fatal("HostDir volume must pin")
	}
}

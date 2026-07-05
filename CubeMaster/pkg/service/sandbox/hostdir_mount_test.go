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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, _ := json.Marshal(tt.opts)
			req := &types.CreateCubeSandboxReq{
				Annotations: map[string]string{
					AnnotationHostDirMount: string(raw),
				},
			}
			err := injectHostDirMounts(context.Background(), req)
			if (err != nil) != tt.wantErr {
				t.Errorf("injectHostDirMounts() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateHostPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"under allowed prefix", "/data/shared/foo", false},
		{"exact allowed dir", "/data/shared", false},
		{"traversal escape", "/data/shared/../secret", true},
		{"unrelated path", "/tmp/data", true},
		{"prefix spoof", "/data/shared_hack/x", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHostPath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateHostPath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
		})
	}
}

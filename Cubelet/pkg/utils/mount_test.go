// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestIsMountPoint(t *testing.T) {
	type args struct {
		dst string
	}
	tests := []struct {
		name    string
		args    args
		want    bool
		wantErr bool
	}{
		{
			name:    "test1",
			args:    args{"/data/docker/lib/overlay2/a44f8ec9b040f22b763fae4f9ee5732852513beab3c5da48a2d809f32e103ff7/merged"},
			want:    false,
			wantErr: true,
		}, {
			name: "test2",
			args: args{"/sys"},
			want: true,
		},
		{
			name: "test2",
			args: args{"/proc"},
			want: true,
		},
		{
			name: "test3",
			args: args{"/dev"},
			want: true,
		},
		{
			name: "test4",
			args: args{t.TempDir()},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := IsMountPoint(tt.args.dst)
			if (err != nil) != tt.wantErr {
				t.Errorf("IsMountPoint() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("IsMountPoint() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsMountPointViaSymlinkedAncestorAndTrailingSlash(t *testing.T) {
	SkipUnlessRootWithSysAdmin(t)

	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "src")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	realTarget := filepath.Join(tmp, "real", "mnt")
	if err := os.MkdirAll(realTarget, 0755); err != nil {
		t.Fatal(err)
	}
	linkBase := filepath.Join(tmp, "link")
	if err := os.Symlink(filepath.Join(tmp, "real"), linkBase); err != nil {
		t.Fatal(err)
	}

	if err := unix.Mount(srcDir, realTarget, "", unix.MS_BIND, ""); err != nil {
		t.Fatalf("bind mount: %v", err)
	}
	t.Cleanup(func() {
		_ = unix.Unmount(realTarget, unix.MNT_DETACH)
	})

	for _, candidate := range []string{
		realTarget,
		realTarget + "/",
		filepath.Join(linkBase, "mnt"),
	} {
		got, err := IsMountPoint(candidate)
		if err != nil {
			t.Fatalf("IsMountPoint(%q) error = %v", candidate, err)
		}
		if !got {
			t.Fatalf("IsMountPoint(%q) = false, want true", candidate)
		}
	}
}

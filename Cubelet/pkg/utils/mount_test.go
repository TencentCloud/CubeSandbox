// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"testing"
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

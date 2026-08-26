// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cow

import "testing"

func TestNormalizeBackend(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", BackendXFS, false},
		{"xfs", BackendXFS, false},
		{"XFS", BackendXFS, false},
		{"xfscow", BackendXFS, false},
		{"cubecow", BackendXFS, false},
		{"s3", BackendS3, false},
		{"S3", BackendS3, false},
		{"cos", "", true},
	}
	for _, tc := range cases {
		got, err := NormalizeBackend(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("NormalizeBackend(%q) err=nil, want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("NormalizeBackend(%q) unexpected err: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("NormalizeBackend(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

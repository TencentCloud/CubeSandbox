// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package constants

import "testing"

func TestNormalizeSnapshotBackend(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: SnapshotBackendXFS},
		{in: "xfs", want: SnapshotBackendXFS},
		{in: "XFS", want: SnapshotBackendXFS},
		{in: "cubecow", want: SnapshotBackendXFS},
		{in: "s3", want: SnapshotBackendS3},
		{in: "S3", want: SnapshotBackendS3},
		{in: "nfs", wantErr: true},
	}
	for _, tc := range cases {
		got, err := NormalizeSnapshotBackend(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("NormalizeSnapshotBackend(%q) err=nil, want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("NormalizeSnapshotBackend(%q) err=%v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("NormalizeSnapshotBackend(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveSnapshotBackend(t *testing.T) {
	t.Parallel()
	got, err := ResolveSnapshotBackend("", "  ", "s3")
	if err != nil {
		t.Fatalf("ResolveSnapshotBackend err=%v", err)
	}
	if got != SnapshotBackendS3 {
		t.Fatalf("ResolveSnapshotBackend=%q, want s3", got)
	}
	got, err = ResolveSnapshotBackend("", "")
	if err != nil {
		t.Fatalf("empty ResolveSnapshotBackend err=%v", err)
	}
	if got != SnapshotBackendXFS {
		t.Fatalf("empty ResolveSnapshotBackend=%q, want xfs", got)
	}
}

func TestOptionalSnapshotBackend(t *testing.T) {
	t.Parallel()
	got, ok, err := OptionalSnapshotBackend("", "")
	if err != nil || ok || got != "" {
		t.Fatalf("empty OptionalSnapshotBackend got=(%q,%v,%v)", got, ok, err)
	}
	got, ok, err = OptionalSnapshotBackend("", "s3")
	if err != nil || !ok || got != SnapshotBackendS3 {
		t.Fatalf("OptionalSnapshotBackend s3 got=(%q,%v,%v)", got, ok, err)
	}
}

func TestSnapshotRemoteStatus(t *testing.T) {
	t.Parallel()
	if got := SnapshotRemoteStatus(SnapshotBackendS3); got != RemoteStatusPending {
		t.Fatalf("s3 remote_status=%q, want %q", got, RemoteStatusPending)
	}
	if got := SnapshotRemoteStatus(SnapshotBackendXFS); got != "" {
		t.Fatalf("xfs remote_status=%q, want empty", got)
	}
}

func TestIsS3Backend(t *testing.T) {
	t.Parallel()
	if !IsS3Backend("s3") || !IsS3Backend("S3") {
		t.Fatal("s3 must be IsS3Backend")
	}
	if IsS3Backend("") || IsS3Backend("xfs") || IsS3Backend("cubecow") || IsS3Backend("nfs") {
		t.Fatal("non-s3 must not be IsS3Backend")
	}
}

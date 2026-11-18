// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package cubesandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"testing"
	"time"
)

func TestIntegrationFilesWriteFrom(t *testing.T) {
	cfg := integrationConfig(t)
	client := NewClient(cfg)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	sb := createIntegrationSandbox(t, ctx, client, CreateOptions{
		Timeout: DurationPtr(2 * time.Minute),
		Metadata: map[string]string{
			"sdk": "go", "scenario": "integration-streaming-upload",
		},
	})

	for _, tc := range []struct {
		name      string
		path      string
		user      string
		size      int64
		overwrite bool
	}{
		{name: "empty"},
		{name: "empty_overwrite", overwrite: true},
		{name: "binary", size: 8 << 20},
		{name: "query_path", path: "/tmp/go sdk write-from ?#%\".bin", size: 1024},
		{name: "root", user: "root", size: 64 << 10},
		{name: "nobody", user: "nobody", size: 64 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			if path == "" {
				path = "/tmp/go-sdk-write-from-" + tc.name + ".bin"
			}
			files := sb.Files().ForUser(tc.user)
			if tc.overwrite {
				if err := files.Write(ctx, path, []byte("content to truncate")); err != nil {
					t.Fatalf("seed destination: %v", err)
				}
			}
			if err := files.WriteFrom(ctx, path, &uploadPatternReader{remaining: tc.size}); err != nil {
				t.Fatalf("WriteFrom: %v", err)
			}
			stream, err := files.ReadStream(ctx, path)
			if err != nil {
				t.Fatalf("ReadStream: %v", err)
			}
			defer stream.Close()
			got := sha256.New()
			n, err := io.Copy(got, stream)
			if err != nil || n != tc.size {
				t.Fatalf("downloaded=%d want=%d err=%v", n, tc.size, err)
			}
			want := sha256.New()
			_, _ = io.Copy(want, &uploadPatternReader{remaining: tc.size})
			if !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
				t.Fatal("uploaded and downloaded hashes differ")
			}
			entry, err := files.Stat(ctx, path)
			if err != nil {
				t.Fatalf("Stat destination from path query: %v", err)
			}
			if entry.Size != tc.size {
				t.Fatalf("size=%d want=%d", entry.Size, tc.size)
			}
			if tc.user != "" && entry.Owner != tc.user {
				t.Fatalf("owner=%q want=%q", entry.Owner, tc.user)
			}
		})
	}
}

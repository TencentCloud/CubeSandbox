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
		name string
		user string
		size int64
	}{
		{name: "empty"},
		{name: "binary", size: 8 << 20},
		{name: "root", user: "root", size: 64 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/tmp/go-sdk-write-from-" + tc.name + ".bin"
			files := sb.Files().ForUser(tc.user)
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
			if tc.user != "" {
				entry, err := files.Stat(ctx, path)
				if err != nil {
					t.Fatalf("Stat: %v", err)
				}
				if entry.Owner != tc.user {
					t.Fatalf("owner=%q want=%q", entry.Owner, tc.user)
				}
			}
		})
	}
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
)

func TestOpenS3AddressingStyle(t *testing.T) {
	legacyFalse := false
	for _, tc := range []struct {
		name, style, nodeEndpoint, host, path string
		legacyPathStyle                       *bool
	}{
		{name: "legacy default", nodeEndpoint: "https://downloads.example.test", host: "downloads.example.test", path: "/artifacts/nested/a.ext4"},
		{name: "legacy false", legacyPathStyle: &legacyFalse, nodeEndpoint: "https://downloads.example.test", host: "downloads.example.test", path: "/artifacts/nested/a.ext4"},
		{name: "auto overrides default", style: "auto", nodeEndpoint: "https://s3.dualstack.us-east-1.amazonaws.com", host: "artifacts.s3.dualstack.us-east-1.amazonaws.com", path: "/nested/a.ext4"},
		{name: "path overrides false", style: "path", legacyPathStyle: &legacyFalse, nodeEndpoint: "https://s3.dualstack.us-east-1.amazonaws.com", host: "s3.dualstack.us-east-1.amazonaws.com", path: "/artifacts/nested/a.ext4"},
		{name: "virtual overrides default", style: "virtual", nodeEndpoint: "https://downloads.example.test", host: "artifacts.downloads.example.test", path: "/nested/a.ext4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := OpenS3(config.S3Config{
				Endpoint:        "https://storage.example.test",
				NodeEndpoint:    tc.nodeEndpoint,
				AccessKeyID:     "test-access-key",
				SecretAccessKey: "test-secret-key",
				Bucket:          "artifacts",
				Region:          "us-east-1",
				PathStyle:       tc.legacyPathStyle,
				AddressingStyle: tc.style,
			}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			defer st.(*Adapter).Store.Close()
			raw, err := st.PresignGet(t.Context(), "nested/a.ext4", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if u.Host != tc.host || u.Path != tc.path {
				t.Fatalf("signed URL host=%q path=%q, want %q/%q", u.Host, u.Path, tc.host, tc.path)
			}
		})
	}
}

func TestOpenS3RejectsInvalidAddressingStyle(t *testing.T) {
	_, err := OpenS3(config.S3Config{
		Endpoint:        "https://storage.example.test",
		AccessKeyID:     "test-access-key",
		SecretAccessKey: "test-secret-key",
		Bucket:          "artifacts",
		AddressingStyle: "invalid",
	}, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "addressing_style") {
		t.Fatalf("invalid style must fail at OpenS3, got %v", err)
	}
}

func TestOpenFSSignedURLUsesObjectMountPath(t *testing.T) {
	key := hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32))
	st, err := OpenFS(config.StoreFSBackendConfig{
		Root:       t.TempDir(),
		PublicURL:  "http://10.0.0.1:3010",
		SigningKey: key,
	}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.EnsureBucket(ctx); err != nil {
		t.Fatal(err)
	}
	objKey := ObjectKey("amd64", "cube-shim", "v1")
	if _, err := st.Put(ctx, objKey, bytes.NewReader([]byte("x")), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	raw, err := st.PresignGet(ctx, objKey, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != ObjectMountPath {
		t.Fatalf("signed path %q want %q", u.Path, ObjectMountPath)
	}
}

func TestOpenFSSharedCapability(t *testing.T) {
	key := hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32))
	st, err := OpenFS(config.StoreFSBackendConfig{
		Root:       t.TempDir(),
		PublicURL:  "http://10.0.0.1:3010",
		SigningKey: key,
		Shared:     true,
	}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ad, ok := st.(*Adapter)
	if !ok {
		t.Fatalf("type %T", st)
	}
	if !ad.Store.Capabilities().Shared {
		t.Fatal("Shared=true must set driver capability")
	}
}

func TestWarehouseRetentionRules(t *testing.T) {
	rules := warehouseRetention()
	if len(rules) != 2 {
		t.Fatalf("rules=%d want 2", len(rules))
	}
	if rules[0].Prefix != Prefix || rules[0].AbortIncompleteAfter != 24*time.Hour || rules[0].ExpireAfter != 0 {
		t.Fatalf("abort rule = %+v", rules[0])
	}
	if rules[1].Prefix != uploadsPrefix || rules[1].ExpireAfter != 24*time.Hour {
		t.Fatalf("expire rule = %+v", rules[1])
	}
}

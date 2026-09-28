// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/url"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/config"
)

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

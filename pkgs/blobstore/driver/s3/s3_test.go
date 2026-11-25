// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package s3

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
)

func TestParseEndpoint(t *testing.T) {
	host, secure, err := parseEndpoint("minio:9000", "false")
	if err != nil || host != "minio:9000" || secure {
		t.Fatalf("%s %v %v", host, secure, err)
	}
	host, secure, err = parseEndpoint("https://s3.amazonaws.com", "")
	if err != nil || host != "s3.amazonaws.com" || !secure {
		t.Fatalf("%s %v %v", host, secure, err)
	}
	if _, _, err := parseEndpoint("https://s3.example.com/bucket", ""); err == nil {
		t.Fatal("path-prefixed endpoint must fail")
	}
}

func TestOptionsConfig(t *testing.T) {
	cfg := Options{
		Endpoint:        "http://minio:9000",
		AccessKeyID:     "ak",
		SecretAccessKey: "sk",
		Bucket:          "cube-ops",
		PathStyle:       true,
		CreateBucket:    true,
	}.Config("warehouse")
	if cfg.Driver != DriverName || cfg.Namespace != "cube-ops" || cfg.Prefix != "warehouse" {
		t.Fatalf("%+v", cfg)
	}
}

func TestOpenRequiresCreds(t *testing.T) {
	_, err := blobstore.Open(t.Context(), blobstore.Config{Driver: DriverName, Namespace: "b"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestPutContextSurvivesCallerCancel(t *testing.T) {
	s := &store{putTimeout: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	putCtx, stop := s.putContext(ctx)
	defer stop()
	if putCtx.Err() != nil {
		t.Fatal("put ctx must not inherit caller cancel so abort MPU can run")
	}
	deadline, ok := putCtx.Deadline()
	if !ok || time.Until(deadline) > time.Minute || time.Until(deadline) < 0 {
		t.Fatalf("unexpected deadline %v ok=%v", deadline, ok)
	}
}

func TestPrepareDoesNotWriteBucketLifecycle(t *testing.T) {
	var headCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == "/shared-bucket/" && r.URL.RawQuery == "" {
			headCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))
	defer server.Close()

	cfg := Options{
		Endpoint:        server.URL,
		AccessKeyID:     "test-ak",
		SecretAccessKey: "test-sk",
		Bucket:          "shared-bucket",
		Region:          "us-east-1",
		PathStyle:       true,
	}.Config("warehouse")
	cfg.Retention = []blobstore.RetentionRule{
		{Prefix: "", AbortIncompleteAfter: 24 * time.Hour},
		{Prefix: "uploads/", ExpireAfter: 24 * time.Hour},
	}

	st, err := blobstore.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := headCalls.Load(); got != 1 {
		t.Fatalf("HEAD bucket calls = %d, want 1", got)
	}
}

func TestSHA256UserMetadataSkipsCopy(t *testing.T) {
	if sha256UserMetadata("") != nil {
		t.Fatal("empty digest must not set metadata (and must not CopyObject)")
	}
	if sha256UserMetadata("sha256:") != nil {
		t.Fatal("empty sha256: prefix must not set metadata")
	}
	got := sha256UserMetadata("sha256:deadbeef")
	if got[metaSHA256Key] != "deadbeef" {
		t.Fatalf("got %#v", got)
	}
	got = sha256UserMetadata("cafe")
	if got[metaSHA256Key] != "cafe" {
		t.Fatalf("got %#v", got)
	}
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package s3api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/config"
)

// A registry can change between the SDK's initial HEAD and conditional GET.
// ReadClaims must return the body and ETag from one snapshot, even if another
// writer changes the object before or after that GET.
func TestReadClaimsConcurrentRegistryUpdate(t *testing.T) {
	const body = `{"version":1,"claims":[]}`
	var heads, gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified", "Mon, 02 Nov 2026 00:00:00 GMT")
		switch r.Method {
		case http.MethodHead:
			heads.Add(1)
			// The registry changes immediately after this HEAD.
			w.Header().Set("ETag", `"before-update"`)
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		case http.MethodGet:
			gets.Add(1)
			if r.Header.Get("If-Match") == `"before-update"` {
				w.WriteHeader(http.StatusPreconditionFailed)
				fmt.Fprint(w, `<Error><Code>PreconditionFailed</Code></Error>`)
				return
			}
			w.Header().Set("ETag", `"get-snapshot"`)
			fmt.Fprint(w, body)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	client := claimsTestClient(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, etag, err := client.ReadClaims(ctx, ".cube-sandbox/volume-claims.json")
	if err != nil {
		t.Fatalf("ReadClaims during update: %v", err)
	}
	if string(data) != body || etag != "get-snapshot" {
		t.Fatalf("body=%q ETag=%q, want the GET snapshot", data, etag)
	}
	if heads.Load() != 0 || gets.Load() != 1 {
		t.Fatalf("HEAD=%d GET=%d, want one snapshot read without a separate metadata request", heads.Load(), gets.Load())
	}
}

func claimsTestClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	client, err := NewFor(context.Background(), Options{
		Bucket: "test-bucket", Endpoint: endpoint, Region: "us-east-1",
		Addressing: config.AddressingPath, AccessKeyID: "test-key", SecretAccessKey: "test-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestReadClaimsBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		length    int
		wantETag  string
		wantError bool
	}{
		{name: "empty object", status: 200, wantETag: "snapshot"},
		{name: "missing object", status: 404, body: `<Error><Code>NoSuchKey</Code></Error>`},
		{name: "missing bucket", status: 404, body: `<Error><Code>NoSuchBucket</Code></Error>`},
		{name: "access denied", status: 403, body: `<Error><Code>AccessDenied</Code></Error>`, wantError: true},
		{name: "truncated read", status: 200, body: "short", length: 100, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"snapshot"`)
				w.Header().Set("Last-Modified", "Mon, 02 Nov 2026 00:00:00 GMT")
				if tc.length != 0 {
					w.Header().Set("Content-Length", fmt.Sprint(tc.length))
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			data, etag, err := claimsTestClient(t, srv.URL).ReadClaims(ctx, "claims.json")
			if tc.wantError {
				if err == nil || data != nil || etag != "" {
					t.Fatalf("data=%q ETag=%q err=%v, want a failed read without usable state", data, etag, err)
				}
				if tc.status == 403 && !strings.Contains(strings.ToLower(err.Error()), "denied") {
					t.Fatalf("error=%v, want access denied", err)
				}
				return
			}
			if err != nil || len(data) != 0 || etag != tc.wantETag {
				t.Fatalf("data=%q ETag=%q err=%v, want empty data and ETag=%q", data, etag, err, tc.wantETag)
			}
		})
	}
}

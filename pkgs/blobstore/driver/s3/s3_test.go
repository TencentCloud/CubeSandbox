// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package s3

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/tencentcloud/CubeSandbox/pkgs/blobstore"
)

func TestAddressingStyleVirtual(t *testing.T) {
	cfg := Options{
		Endpoint:        "https://storage.example.test",
		NodeEndpoint:    "https://downloads.example.test",
		AccessKeyID:     "test-access-key",
		SecretAccessKey: "test-secret-key",
		Bucket:          "artifacts",
		Region:          "us-east-1",
		PathStyle:       true,
	}.Config("templates")
	cfg.Extra["addressing_style"] = "virtual"
	st, err := blobstore.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	raw, err := st.SignedGetURL(t.Context(), "nested/a.ext4", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "artifacts.downloads.example.test" || u.Path != "/templates/nested/a.ext4" {
		t.Fatalf("virtual addressing was not applied to the signed download URL: host=%q path=%q", u.Host, u.Path)
	}
}

func TestSignedGetURLAddressingModes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		style       string
		legacy      bool
		omitLegacy  bool
		endpoint    string
		wantVirtual bool
	}{
		{name: "legacy unset", omitLegacy: true},
		{name: "legacy path", legacy: true},
		{name: "legacy auto"},
		{name: "virtual overrides path", style: "virtual", legacy: true, wantVirtual: true},
		{name: "path overrides auto", style: "path"},
		{name: "normalize explicit style", style: " Virtual ", wantVirtual: true},
		{name: "auto overrides path on AWS", style: "auto", legacy: true, endpoint: "https://s3.amazonaws.com", wantVirtual: true},
		{name: "legacy auto on AWS", endpoint: "https://s3.amazonaws.com", wantVirtual: true},
		{name: "legacy unset on AWS", omitLegacy: true, endpoint: "https://s3.amazonaws.com"},
		{name: "explicit path on AWS", style: "path", endpoint: "https://s3.amazonaws.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := tc.endpoint
			if endpoint == "" {
				endpoint = "https://storage.example.test"
			}
			cfg := Options{
				Endpoint: endpoint, Bucket: "artifacts", Region: "us-east-1",
				AccessKeyID: "test-access-key", SecretAccessKey: "test-secret-key",
				PathStyle: tc.legacy, AddressingStyle: tc.style,
			}.Config("templates")
			if tc.omitLegacy {
				delete(cfg.Extra, "path_style")
			}
			st, err := blobstore.Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			raw, err := st.SignedGetURL(t.Context(), "a.ext4", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			wantPath := "/artifacts/templates/a.ext4"
			if tc.wantVirtual {
				wantPath = "/templates/a.ext4"
			}
			if u.Path != wantPath || strings.HasPrefix(u.Host, "artifacts.") != tc.wantVirtual {
				t.Fatalf("addressing_style=%q path_style=%v: host=%q path=%q; want virtual=%v path=%q", tc.style, tc.legacy, u.Host, u.Path, tc.wantVirtual, wantPath)
			}
			if u.Query().Get("X-Amz-Signature") == "" {
				t.Fatal("expected a signed URL")
			}
		})
	}
}

func TestOpenRejectsInvalidAddressingStyle(t *testing.T) {
	cfg := Options{
		Endpoint: "https://storage.example.test", Bucket: "artifacts", Region: "us-east-1",
		AccessKeyID: "test-access-key", SecretAccessKey: "test-secret-key",
		AddressingStyle: " Vritual ",
	}.Config("")
	_, err := blobstore.Open(t.Context(), cfg)
	if err == nil || !strings.Contains(err.Error(), "addressing_style") || !strings.Contains(err.Error(), `" Vritual "`) {
		t.Fatalf("invalid explicit addressing mode must fail with a diagnostic: %v", err)
	}
}

func TestOpenRejectsVirtualAddressingForIPLiteral(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, nodeEndpoint, want string
	}{
		{"data IPv4", "http://192.0.2.1:9000", "", "blobstore/s3: data endpoint:"},
		{"presign IPv6", "https://storage.example.test", "http://[2001:db8::1]:9000", "presign endpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Options{
				Endpoint: tc.endpoint, NodeEndpoint: tc.nodeEndpoint,
				Bucket: "artifacts", AccessKeyID: "test-key", SecretAccessKey: "test-secret",
				AddressingStyle: " Virtual ",
			}.Config("templates")
			if _, err := blobstore.Open(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "requires a DNS hostname") {
				t.Fatalf("expected clear error for virtual IP endpoint, got %v", err)
			}
			cfg.Extra[extraAddressingStyle] = "path"
			store, err := blobstore.Open(t.Context(), cfg)
			if err != nil {
				t.Fatalf("path addressing should accept the same endpoint: %v", err)
			}
			defer store.Close()
		})
	}
}

func TestStatUsesConfiguredAddressingStyle(t *testing.T) {
	// Keep this test and other minio-client tests non-parallel: this fixture
	// temporarily replaces the package-global minio.DefaultTransport.
	for _, tc := range []struct {
		style, host, path string
	}{
		{"virtual", "artifacts.storage.example.test", "/templates/a.ext4"},
		{"path", "storage.example.test", "/artifacts/templates/a.ext4"},
		{"auto", "storage.example.test", "/artifacts/templates/a.ext4"},
	} {
		t.Run(tc.style, func(t *testing.T) {
			type requestTarget struct{ method, host, path string }
			seen := make(chan requestTarget, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case seen <- requestTarget{r.Method, r.Host, r.URL.Path}:
				default:
				}
				w.Header().Set("Last-Modified", "Mon, 16 Nov 2026 00:00:00 GMT")
				w.Header().Set("Content-Length", "0")
				w.Header().Set("ETag", `"test-etag"`)
			}))
			defer server.Close()

			// Keep actual minio-go request construction, but route its TCP
			// connections to the local fixture without resolving external DNS.
			original := minio.DefaultTransport
			t.Cleanup(func() { minio.DefaultTransport = original })
			minio.DefaultTransport = func(secure bool) (*http.Transport, error) {
				transport, err := original(secure)
				if err != nil {
					return nil, err
				}
				transport.Proxy = nil
				transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
				}
				t.Cleanup(transport.CloseIdleConnections)
				return transport, nil
			}
			st, err := blobstore.Open(t.Context(), Options{
				Endpoint: "http://storage.example.test", Bucket: "artifacts", Region: "us-east-1",
				AccessKeyID: "test-access-key", SecretAccessKey: "test-secret-key",
				PathStyle: true, AddressingStyle: tc.style,
			}.Config("templates"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if _, err := st.Stat(ctx, "a.ext4"); err != nil {
				t.Fatal(err)
			}
			got := <-seen
			if got.method != http.MethodHead || got.host != tc.host || got.path != tc.path {
				t.Fatalf("actual S3 request=%+v, want HEAD host=%q path=%q", got, tc.host, tc.path)
			}
		})
	}
}

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

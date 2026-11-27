// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package s3api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/minio/minio-go/v7"

	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/config"
)

func testClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	client, err := NewFor(context.Background(), Options{
		Bucket: "test-bucket", Endpoint: endpoint, Region: "us-east-1",
		Addressing: config.AddressingPath, AccessKeyID: "test-ak", SecretAccessKey: "test-sk",
	})
	if err != nil {
		t.Fatalf("NewFor: %v", err)
	}
	return client
}

func TestPrefixHasDataDistinguishesEmptyDirectoryMarker(t *testing.T) {
	for _, tc := range []struct {
		key  string
		size int
		want bool
	}{
		{"workspace/", 0, false},
		{"workspace/", 1, true},
		{"workspace/file", 0, true},
	} {
		t.Run(fmt.Sprintf("%s-%d", tc.key, tc.size), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `<ListBucketResult><Name>test-bucket</Name><IsTruncated>false</IsTruncated><Contents><Key>%s</Key><Size>%d</Size></Contents></ListBucketResult>`, tc.key, tc.size)
			}))
			defer srv.Close()
			got, err := testClient(t, srv.URL).PrefixHasData(context.Background(), "workspace/")
			if err != nil || got != tc.want {
				t.Fatalf("hasData=%v err=%v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestCreatePrefixWritesS3fsDirectoryMarker(t *testing.T) {
	var got http.Header
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			got, gotPath = r.Header.Clone(), r.URL.Path
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := testClient(t, srv.URL).CreatePrefix(context.Background(), "volumes/mydata/"); err != nil {
		t.Fatalf("CreatePrefix: %v", err)
	}
	if gotPath != "/test-bucket/volumes/mydata/" {
		t.Fatalf("PUT path = %q", gotPath)
	}
	if ct := got.Get("Content-Type"); ct != "application/x-directory" {
		t.Errorf("Content-Type = %q, want application/x-directory", ct)
	}
	if mode := got.Get("X-Amz-Meta-Mode"); mode != "16877" {
		t.Errorf("x-amz-meta-mode = %q, want 16877 (S_IFDIR|0755)", mode)
	}
	for _, h := range []string{"Uid", "Gid", "Atime", "Ctime", "Mtime"} {
		if got.Get("X-Amz-Meta-"+h) == "" {
			t.Errorf("x-amz-meta-%s missing: s3fs rejects a mount-point marker without it", strings.ToLower(h))
		}
	}
}

func TestRemovePrefixDistinguishesDestroyOutcomes(t *testing.T) {
	const objects = `<ListBucketResult><Name>test-bucket</Name><IsTruncated>false</IsTruncated><Contents><Key>workspace/one</Key><Size>1</Size></Contents><Contents><Key>workspace/two</Key><Size>1</Size></Contents></ListBucketResult>`
	const rejected = `<Error><Key>workspace/two</Key><Code>AccessDenied</Code><Message>rejected</Message></Error>`
	const deleted = `<Deleted><Key>workspace/one</Key></Deleted>`
	const unknown = `<Error><Key>workspace/one</Key><Code>InternalError</Code><Message>unknown</Message></Error>`
	const throttle = `<Error><Code>SlowDown</Code></Error>`
	const forbidden = `<Error><Code>AccessDenied</Code></Error>`
	type reply struct {
		status int
		body   string
	}
	for _, tc := range []struct {
		name      string
		list      reply
		retryList reply
		deletes   []reply
		want      error
		unknown   bool
	}{
		{name: "confirmed missing bucket", list: reply{404, `<Error><Code>NoSuchBucket</Code><BucketName>test-bucket</BucketName></Error>`}},
		{name: "different missing bucket", list: reply{404, `<Error><Code>NoSuchBucket</Code><BucketName>other</BucketName></Error>`}, want: errUnverifiedNotFound},
		{name: "gateway 404 with request ID", list: reply{404, "route not found"}, want: errUnverifiedNotFound},
		{name: "gateway 503 while listing", list: reply{503, "upstream unavailable"}, want: errUnverified503},
		{name: "initial list failed before deletion", list: reply{403, forbidden}, want: ErrDestroyNotStarted},
		{name: "partial delete acknowledged", deletes: []reply{{200, "<DeleteResult>" + deleted + rejected + "</DeleteResult>"}}, want: ErrDestroyRetryable},
		{name: "unknown object outcome takes precedence", deletes: []reply{{200, "<DeleteResult>" + rejected + unknown + "</DeleteResult>"}}, unknown: true},
		{name: "bare 503 while deleting", deletes: []reply{{503, "upstream unavailable"}}, want: errUnverified503},
		{name: "connection dropped while deleting", deletes: []reply{{}}, unknown: true},
		{name: "SlowDown recovers with one retry", deletes: []reply{{503, throttle}, {200, "<DeleteResult>" + deleted + "<Deleted><Key>workspace/two</Key></Deleted></DeleteResult>"}}},
		{name: "SlowDown still retryable after exhaustion", deletes: []reply{{503, throttle}, {503, throttle}}, want: ErrDestroyRetryable},
		{name: "failed relist after SlowDown stays delete-only", retryList: reply{403, forbidden}, deletes: []reply{{503, throttle}}, want: ErrDestroyRetryable},
		{name: "unknown outcome on retry", deletes: []reply{{503, throttle}, {}}, unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deletes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				result := tc.list
				if r.Method == http.MethodPost {
					if _, err := io.Copy(io.Discard, r.Body); err != nil {
						t.Error(err)
						return
					}
					index := int(deletes.Add(1)) - 1
					if index >= len(tc.deletes) {
						t.Error("unexpected extra delete")
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					result = tc.deletes[index]
					if result.status == 0 {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
						} else {
							conn.Close()
						}
						return
					}
				} else if tc.retryList.status != 0 && deletes.Load() > 0 {
					result = tc.retryList
				} else if result.status == 0 {
					result = reply{200, objects}
				}
				w.Header().Set("x-amz-request-id", "test-request")
				if strings.HasPrefix(result.body, "<") {
					w.Header().Set("Content-Type", "application/xml")
				}
				w.WriteHeader(result.status)
				fmt.Fprint(w, result.body)
			}))
			defer srv.Close()
			client, err := NewFor(t.Context(), Options{
				Bucket: "test-bucket", Endpoint: srv.URL, Region: "us-east-1", MaxAttempts: 1,
				Addressing: config.AddressingPath, AccessKeyID: "test-ak", SecretAccessKey: "test-sk",
			})
			if err != nil {
				t.Fatal(err)
			}
			err = client.RemovePrefix(t.Context(), "workspace/")
			if tc.want != ErrDestroyNotStarted && errors.Is(err, ErrDestroyNotStarted) {
				t.Fatalf("RemovePrefix unexpectedly released the delete claim: %v", err)
			}
			switch {
			case tc.want != nil:
				if !errors.Is(err, tc.want) {
					t.Fatalf("RemovePrefix error = %v, want %v", err, tc.want)
				}
			case tc.unknown:
				if err == nil || errors.Is(err, ErrDestroyRetryable) || errors.Is(err, ErrDestroyNotStarted) {
					t.Fatalf("RemovePrefix error = %v, want unknown outcome", err)
				}
			default:
				if err != nil {
					t.Fatalf("RemovePrefix error = %v, want success", err)
				}
			}
			if got := int(deletes.Load()); got != len(tc.deletes) {
				t.Fatalf("delete requests = %d, want %d", got, len(tc.deletes))
			}
		})
	}
}

func TestParseEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name       string
		endpoint   string
		wantHost   string
		wantSecure bool
	}{
		{"https", "https://s3.us-east-1.amazonaws.com", "s3.us-east-1.amazonaws.com", true},
		{"http with port", "http://minio:9000", "minio:9000", false},
		{"trailing slash", "https://cos.ap-guangzhou.myqcloud.com/", "cos.ap-guangzhou.myqcloud.com", true},
		{"no scheme defaults to tls", "s3.example.com", "s3.example.com", true},
		{"r2", "https://acct.r2.cloudflarestorage.com", "acct.r2.cloudflarestorage.com", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, secure, err := ParseEndpoint(tc.endpoint)
			if err != nil {
				t.Fatalf("ParseEndpoint(%q): %v", tc.endpoint, err)
			}
			if host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
			if secure != tc.wantSecure {
				t.Errorf("secure = %v, want %v", secure, tc.wantSecure)
			}
		})
	}
}

func TestParseEndpointErrors(t *testing.T) {
	for _, endpoint := range []string{"", "   ", "ftp://example.com", "http://", "https://host/s3proxy"} {
		if _, _, err := ParseEndpoint(endpoint); err == nil {
			t.Errorf("ParseEndpoint(%q) succeeded, want error", endpoint)
		}
	}
}

// AWS rejects a LocationConstraint for us-east-1 and R2 (REGION=auto) rejects it
// outright; minio-go omits the element exactly when the location is us-east-1.
func TestBucketCreateRegion(t *testing.T) {
	for _, tc := range []struct {
		region string
		want   string
	}{
		{"us-east-1", "us-east-1"},
		{"auto", "us-east-1"},
		{"", "us-east-1"},
		{"ap-guangzhou", "ap-guangzhou"},
		{"eu-central-1", "eu-central-1"},
	} {
		if got := BucketCreateRegion(tc.region); got != tc.want {
			t.Errorf("BucketCreateRegion(%q) = %q, want %q", tc.region, got, tc.want)
		}
	}
}

// Two concurrent first-time creates race on MakeBucket; the loser must not fail
// the volume create.
func TestIsBucketAlreadyExists(t *testing.T) {
	for _, code := range []string{"BucketAlreadyOwnedByYou", "BucketAlreadyExists"} {
		if !IsBucketAlreadyExists(minio.ErrorResponse{Code: code}) {
			t.Errorf("IsBucketAlreadyExists(%s) = false, want true", code)
		}
	}
	for _, code := range []string{"AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch"} {
		if IsBucketAlreadyExists(minio.ErrorResponse{Code: code}) {
			t.Errorf("IsBucketAlreadyExists(%s) = true, want false", code)
		}
	}
	if IsBucketAlreadyExists(errors.New("dial tcp: connection refused")) {
		t.Error("IsBucketAlreadyExists(non-S3 error) = true, want false")
	}
}

// Destroy tolerates a missing prefix, but every other failure must propagate so
// CubeMaster does not drop the volume record while objects remain.
func TestIsNotFound(t *testing.T) {
	for _, code := range []string{"NoSuchBucket", "NoSuchKey", "NotFound"} {
		if !IsNotFound(minio.ErrorResponse{Code: code}) {
			t.Errorf("IsNotFound(%s) = false, want true", code)
		}
	}
	if !IsNotFound(minio.ErrorResponse{StatusCode: 404}) {
		t.Error("IsNotFound(404) = false, want true")
	}
	for _, code := range []string{"AccessDenied", "InternalError", "SlowDown"} {
		if IsNotFound(minio.ErrorResponse{Code: code}) {
			t.Errorf("IsNotFound(%s) = true, want false", code)
		}
	}
	if IsNotFound(errors.New("dial tcp: connection refused")) {
		t.Error("IsNotFound(non-S3 error) = true, want false")
	}
}

// The point of the instance-role mode: with no keys configured the client
// must ask the node's identity provider, and with keys it must use exactly
// those and never reach for IMDS.
func TestCredentialsFor(t *testing.T) {
	t.Run("static keys are used verbatim", func(t *testing.T) {
		creds := credentialsFor(&config.Config{AccessKeyID: "AK", SecretAccessKey: "SK"})
		value, err := creds.Get()
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if value.AccessKeyID != "AK" || value.SecretAccessKey != "SK" {
			t.Fatalf("Get() = %q/%q, want AK/SK", value.AccessKeyID, value.SecretAccessKey)
		}
		if creds.IsExpired() {
			t.Fatal("static credentials must not start expired")
		}
	})

	t.Run("no keys means the instance role", func(t *testing.T) {
		creds := credentialsFor(&config.Config{Credentials: config.CredentialsInstanceRole})
		// An IAM provider holds nothing until it has asked IMDS, which is what
		// distinguishes it here without making the test reach the network.
		if !creds.IsExpired() {
			t.Fatal("instance-role credentials must start unresolved, not static")
		}
	})
}

// fakeIMDS serves the IMDSv2 token and one role's credentials, or 404 for
// everything when role is empty (an instance with no role attached).
func fakeIMDS(t *testing.T, role string) {
	t.Helper()
	// Keep minio-go on the EC2 path regardless of the test runner's env.
	for _, k := range []string{
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI",
	} {
		t.Setenv(k, "")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const creds = "/latest/meta-data/iam/security-credentials/"
		switch {
		case role == "":
			http.NotFound(w, r)
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			_, _ = w.Write([]byte("token"))
		case r.URL.Path == creds:
			_, _ = w.Write([]byte(role))
		case r.URL.Path == creds+role:
			_, _ = w.Write([]byte(`{"Code":"Success","AccessKeyId":"ASIAROLE","SecretAccessKey":"rolesecret","Token":"session","Expiration":"2099-01-01T00:00:00Z"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	old := imdsEndpoint
	imdsEndpoint = srv.URL
	t.Cleanup(func() { imdsEndpoint = old })
}

var instanceRoleConfig = &config.Config{
	Credentials: config.CredentialsInstanceRole,
	Bucket:      "b",
	Endpoint:    "https://s3.ap-northeast-1.amazonaws.com",
	Region:      "ap-northeast-1",
}

func TestCheckInstanceRole(t *testing.T) {
	t.Run("role attached", func(t *testing.T) {
		fakeIMDS(t, "cube-volumes")
		if err := CheckInstanceRole(context.Background(), instanceRoleConfig); err != nil {
			t.Fatalf("CheckInstanceRole() error = %v", err)
		}
		if _, err := New(context.Background(), instanceRoleConfig); err != nil {
			t.Fatalf("New() error = %v", err)
		}
	})

	t.Run("no role", func(t *testing.T) {
		fakeIMDS(t, "")
		err := CheckInstanceRole(context.Background(), instanceRoleConfig)
		if err == nil || !strings.Contains(err.Error(), "instance metadata service") {
			t.Fatalf("CheckInstanceRole() error = %v, want an IMDS error", err)
		}
		if _, err := New(context.Background(), instanceRoleConfig); err == nil {
			t.Fatal("New() succeeded without a role, want the probe to fail")
		}
	})

	t.Run("static keys never ask IMDS", func(t *testing.T) {
		old := imdsEndpoint
		imdsEndpoint = "http://127.0.0.1:1" // nothing listens here
		t.Cleanup(func() { imdsEndpoint = old })
		cfg := &config.Config{Credentials: config.CredentialsStatic, AccessKeyID: "AK", SecretAccessKey: "SK"}
		if err := CheckInstanceRole(context.Background(), cfg); err != nil {
			t.Fatalf("CheckInstanceRole() error = %v, want nil with static keys", err)
		}
	})
}

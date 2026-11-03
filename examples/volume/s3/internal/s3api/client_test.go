// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package s3api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/minio/minio-go/v7"

	"github.com/tencentcloud/CubeSandbox/examples/volume/s3/internal/config"
)

func TestConditionalClaimWriteHasNoUnconditionalFallback(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		want   error
	}{
		{501, "NotImplemented", ErrConditionalWritesUnsupported},
		{412, "PreconditionFailed", ErrPreconditionFailed},
	} {
		t.Run(tc.code, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPut || r.Header.Get("If-None-Match") != "*" {
					t.Error("registry request lost its create-only condition")
				}
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, "<Error><Code>%s</Code></Error>", tc.code)
			}))
			defer srv.Close()
			err := claimsTestClient(t, srv.URL).WriteClaims(context.Background(), ".cube-sandbox/volume-claims.json", []byte(`{"v":1,"claims":[]}`), "")
			if !errors.Is(err, tc.want) || calls.Load() != 1 {
				t.Fatalf("err=%v requests=%d, want one conditional attempt returning %v", err, calls.Load(), tc.want)
			}
		})
	}
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
			got, err := claimsTestClient(t, srv.URL).PrefixHasData(context.Background(), "workspace/")
			if err != nil || got != tc.want {
				t.Fatalf("hasData=%v err=%v, want %v", got, err, tc.want)
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

// A backend that cannot do the conditional PUT must fail the claim write
// outright. Falling back to a plain PUT would replay a stale registry snapshot
// and silently drop another CubeMaster's claim.
func TestClaimsWriteError(t *testing.T) {
	if err := claimsWriteError("claims.json", nil); err != nil {
		t.Fatalf("claimsWriteError(nil) = %v, want nil", err)
	}
	if err := claimsWriteError("claims.json", minio.ErrorResponse{StatusCode: 412, Code: "PreconditionFailed"}); !errors.Is(err, ErrPreconditionFailed) {
		t.Errorf("claimsWriteError(412) = %v, want ErrPreconditionFailed", err)
	}
	err := claimsWriteError("claims.json", minio.ErrorResponse{StatusCode: 501, Code: "NotImplemented"})
	if !errors.Is(err, ErrConditionalWritesUnsupported) {
		t.Errorf("claimsWriteError(501) = %v, want ErrConditionalWritesUnsupported", err)
	}
	other := claimsWriteError("claims.json", errors.New("access denied"))
	if errors.Is(other, ErrConditionalWritesUnsupported) || !strings.Contains(other.Error(), "access denied") {
		t.Errorf("claimsWriteError(other) = %v, want the original error", other)
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

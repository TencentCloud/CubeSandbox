// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package s3api

import (
	"errors"
	"testing"

	"github.com/minio/minio-go/v7"
)

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

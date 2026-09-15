// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package s3

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
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

func TestIsLifecycleUnsupported(t *testing.T) {
	for _, code := range []string{"MalformedXML", "InvalidRequest", "InvalidArgument"} {
		err := minio.ErrorResponse{Code: code, StatusCode: 400, Message: "schema"}
		if !isLifecycleUnsupported(err) {
			t.Fatalf("code %s should be unsupported", code)
		}
	}
	if isLifecycleUnsupported(minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403}) {
		t.Fatal("AccessDenied must not be treated as unsupported lifecycle")
	}
	if isLifecycleUnsupported(errors.New("connection refused")) {
		t.Fatal("transport error must not be treated as unsupported lifecycle")
	}
}

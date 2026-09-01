// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package warehouse

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
)

func TestParseS3Endpoint(t *testing.T) {
	host, secure, err := parseS3Endpoint("http://minio.ns.svc:9000")
	if err != nil {
		t.Fatal(err)
	}
	if host != "minio.ns.svc:9000" || secure {
		t.Fatalf("host=%s secure=%v", host, secure)
	}
	host, secure, err = parseS3Endpoint("s3.ap-guangzhou.myqcloud.com")
	if err != nil {
		t.Fatal(err)
	}
	if host != "s3.ap-guangzhou.myqcloud.com" || !secure {
		t.Fatalf("host=%s secure=%v", host, secure)
	}
	if _, _, err := parseS3Endpoint("http://minio.local/bucket"); err == nil {
		t.Fatal("path-prefixed endpoint should be rejected")
	}
}

func TestPutContextSurvivesCallerCancel(t *testing.T) {
	s := &S3BlobStore{putTimeout: time.Minute}
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

func TestPutPartSizeConstant(t *testing.T) {
	if PutPartSize != 64<<20 {
		t.Fatalf("PutPartSize=%d want 64MiB so unknown-length uploads do not buffer 512MiB", PutPartSize)
	}
}

func TestObjectKeyUsesWarehousePrefix(t *testing.T) {
	got := ObjectKey(ArchAMD64, ComponentShim, "v0.6.0")
	want := "warehouse/blobs/amd64/cube-shim/v0.6.0/component.tar.gz"
	if got != want {
		t.Fatalf("ObjectKey=%q want %q", got, want)
	}
	if UploadObjectKey("abc") != "warehouse/uploads/abc.tar.gz" {
		t.Fatalf("UploadObjectKey=%q", UploadObjectKey("abc"))
	}
}

func TestLifecycleRulesScopedToPrefix(t *testing.T) {
	s := &S3BlobStore{}
	rules := s.lifecycleRules()
	if len(rules) != 2 {
		t.Fatalf("rules=%d want 2", len(rules))
	}
	if rules[0].RuleFilter.Prefix != Prefix {
		t.Fatalf("abort prefix=%q want %s", rules[0].RuleFilter.Prefix, Prefix)
	}
	if rules[1].RuleFilter.Prefix != uploadsPrefix {
		t.Fatalf("expire prefix=%q want %s", rules[1].RuleFilter.Prefix, uploadsPrefix)
	}
	expireOnly := s.expireUploadsRules()
	if len(expireOnly) != 1 {
		t.Fatalf("expire-only rules=%d want 1", len(expireOnly))
	}
	if expireOnly[0].RuleFilter.Prefix != uploadsPrefix {
		t.Fatalf("expire-only prefix=%q want %s", expireOnly[0].RuleFilter.Prefix, uploadsPrefix)
	}
}

func marshalLifecycle(t *testing.T, rules []lifecycle.Rule) []byte {
	t.Helper()
	cfg := lifecycle.NewConfiguration()
	cfg.Rules = rules
	raw, err := xml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestLifecycleRulesXML(t *testing.T) {
	s := &S3BlobStore{}
	full := marshalLifecycle(t, s.lifecycleRules())
	if !bytes.Contains(full, []byte("AbortIncompleteMultipartUpload")) {
		t.Fatalf("full lifecycle XML missing abort rule: %s", full)
	}
	if !bytes.Contains(full, []byte("warehouse-expire-uploads")) {
		t.Fatalf("full lifecycle XML missing expire rule: %s", full)
	}
	expire := marshalLifecycle(t, s.expireUploadsRules())
	if bytes.Contains(expire, []byte("AbortIncompleteMultipartUpload")) {
		t.Fatalf("expire-only XML must not include abort: %s", expire)
	}
	if !bytes.Contains(expire, []byte("warehouse-expire-uploads")) {
		t.Fatalf("expire-only XML missing expire rule: %s", expire)
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

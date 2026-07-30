// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package reporter

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strconv"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/auth"
)

// TestAuthHeadersSigningMatchesCubeMaster verifies interop with CubeMaster's
// auth package. CubeMaster's CheckSign computes:
//
//	signing_string = "version.userID.timestamp.nonce.sgnMethod"
//	expected_sign  = base64(hmac-sha1(secretKey, signing_string))
//
// The test builds the headers and asserts the signature can be independently
// verified using the same algorithm. This guarantees that when CubeMaster
// receives a POST /event/eviction with these headers, the auth middleware will
// pass (assuming the same secret key is configured).
func TestAuthHeadersSigningMatchesCubeMaster(t *testing.T) {
	userID := "eviction-webhook"
	secretKey := "test-secret-key-cube-master"

	headers, err := auth.Headers(userID, secretKey)
	if err != nil {
		t.Fatalf("auth.Headers: %v", err)
	}

	// Extract header values into SignatureParams (mirroring CubeMaster middleware)
	version := headers.Get("cube_version")
	uid := headers.Get("cube_user_id")
	ts := headers.Get("cube_timestamp")
	nonce := headers.Get("cube_nonce")
	sgnMethod := headers.Get("cube_sgn_method")
	signature := headers.Get("cube_signature")

	if version == "" || uid == "" || ts == "" || nonce == "" || sgnMethod == "" || signature == "" {
		t.Fatalf("missing auth headers: version=%q uid=%q ts=%q nonce=%q method=%q sig=%q",
			version, uid, ts, nonce, sgnMethod, signature)
	}

	// Verify basic properties
	if version != "2023" {
		t.Errorf("expected version=2023, got %s", version)
	}
	if uid != userID {
		t.Errorf("expected uid=%s, got %s", userID, uid)
	}
	if sgnMethod != "sha1" {
		t.Errorf("expected sgnMethod=sha1, got %s", sgnMethod)
	}

	// Verify timestamp is within expected range (±10s, generous for CI)
	timestamp, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		t.Fatalf("timestamp not an integer: %s", ts)
	}
	delta := time.Now().Unix() - timestamp
	if delta < -10 || delta > 10 {
		t.Errorf("timestamp delta %ds out of range (expected ±10s)", delta)
	}

	// Verify nonce is non-empty and numeric
	if _, err := strconv.ParseInt(nonce, 10, 64); err != nil {
		t.Errorf("nonce is not a valid integer: %s", nonce)
	}

	// Recompute the signature and compare (CubeMaster CheckSign logic)
	toSign := version + "." + uid + "." + ts + "." + nonce + "." + sgnMethod
	mac := hmac.New(sha1.New, []byte(secretKey))
	mac.Write([]byte(toSign))
	expectedSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if signature != expectedSig {
		t.Errorf("signature mismatch:\n  computed: %s\n  got:      %s", expectedSig, signature)
	}
}

func TestAuthHeadersDeterministicSignature(t *testing.T) {
	userID := "test-user"
	secretKey := "abc123"

	h, err := auth.Headers(userID, secretKey)
	if err != nil {
		t.Fatalf("auth.Headers: %v", err)
	}

	// Sanity: all 6 headers are present
	expectedHeaders := []string{"cube_version", "cube_user_id", "cube_timestamp", "cube_nonce", "cube_sgn_method", "cube_signature"}
	for _, name := range expectedHeaders {
		if h.Get(name) == "" {
			t.Errorf("header %s is empty", name)
		}
	}
}

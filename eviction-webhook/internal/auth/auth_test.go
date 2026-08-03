// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testUserID    = "test-user"
	testSecretKey = "test-secret-key"
)

// TestHeadersReturnsAllRequiredKeys verifies that Headers returns an http.Header
// containing all six keys that CubeMaster's middleware expects.
func TestHeadersReturnsAllRequiredKeys(t *testing.T) {
	h, err := Headers(testUserID, testSecretKey)
	require.NoError(t, err)

	requiredKeys := []string{
		"Cube_version",
		"Cube_user_id",
		"Cube_timestamp",
		"Cube_nonce",
		"Cube_sgn_method",
		"Cube_signature",
	}
	for _, key := range requiredKeys {
		assert.NotEmpty(t, h.Get(key), "expected header %q to be non-empty", key)
	}
}

// TestHeadersSignatureNotEmpty verifies that the cube_signature header value
// is not empty after calling Headers.
func TestHeadersSignatureNotEmpty(t *testing.T) {
	h, err := Headers(testUserID, testSecretKey)
	require.NoError(t, err)

	sig := h.Get("Cube_signature")
	assert.NotEmpty(t, sig, "expected cube_signature to be non-empty")
}

// TestHeadersDifferentNonceEachCall verifies that two successive calls to
// Headers produce different nonce values (collision probability ≈ 2^-63).
func TestHeadersDifferentNonceEachCall(t *testing.T) {
	h1, err := Headers(testUserID, testSecretKey)
	require.NoError(t, err)

	h2, err := Headers(testUserID, testSecretKey)
	require.NoError(t, err)

	nonce1 := h1.Get("Cube_nonce")
	nonce2 := h2.Get("Cube_nonce")
	require.NotEmpty(t, nonce1, "first nonce must not be empty")
	require.NotEmpty(t, nonce2, "second nonce must not be empty")
	assert.NotEqual(t, nonce1, nonce2, "expected different nonces across calls")
}

// TestHeadersDifferentTimestampOverTime verifies that the cube_timestamp header
// contains a valid numeric Unix timestamp string. We call Headers twice with a
// 1-second sleep and accept either different or equal values — the key
// invariant is that both are valid integer strings.
func TestHeadersDifferentTimestampOverTime(t *testing.T) {
	h1, err := Headers(testUserID, testSecretKey)
	require.NoError(t, err)

	time.Sleep(1100 * time.Millisecond)

	h2, err := Headers(testUserID, testSecretKey)
	require.NoError(t, err)

	ts1 := h1.Get("Cube_timestamp")
	ts2 := h2.Get("Cube_timestamp")

	v1, err1 := strconv.ParseInt(ts1, 10, 64)
	v2, err2 := strconv.ParseInt(ts2, 10, 64)
	require.NoError(t, err1, "cube_timestamp from first call must be a valid integer, got %q", ts1)
	require.NoError(t, err2, "cube_timestamp from second call must be a valid integer, got %q", ts2)

	// After a 1-second sleep the second timestamp must be >= the first.
	assert.GreaterOrEqual(t, v2, v1, "expected second timestamp >= first after 1s sleep")
	// And they should differ by at least 1 second.
	assert.GreaterOrEqual(t, v2-v1, int64(1), "expected timestamps to differ by at least 1s")
}

// TestHeadersAuthEnabled verifies the normal happy path: Headers returns a
// non-nil header map and no error for valid inputs.
func TestHeadersAuthEnabled(t *testing.T) {
	h, err := Headers(testUserID, testSecretKey)
	require.NoError(t, err, "expected no error from Headers with valid inputs")
	require.NotNil(t, h, "expected non-nil http.Header")
}

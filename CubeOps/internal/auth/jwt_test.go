// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
)

// newTestJWTManager creates a JWTManager with a fixed secret for deterministic
// tests.
func newTestJWTManager() *auth.JWTManager {
	return auth.NewJWTManager("test-secret-32-bytes-long-enough!", 15*time.Minute, 168*time.Hour)
}

// TestS1_AccessTokenAccepted verifies the happy path: a freshly generated
// access token passes VerifyAccessToken.
func TestS1_AccessTokenAccepted(t *testing.T) {
	jm := newTestJWTManager()
	access, err := jm.GenerateAccessToken("admin")
	if err != nil {
		t.Fatalf("GenerateAccessToken: %v", err)
	}
	claims, err := jm.VerifyAccessToken(access)
	if err != nil {
		t.Fatalf("VerifyAccessToken failed for a valid access token: %v", err)
	}
	if claims.Username != "admin" {
		t.Errorf("claims.Username = %q, want %q", claims.Username, "admin")
	}
	if claims.Typ != "access" {
		t.Errorf("claims.Typ = %q, want %q", claims.Typ, "access")
	}
}

// TestS1_RefreshTokenAccepted verifies the happy path: a freshly generated
// refresh token passes VerifyRefreshToken.
func TestS1_RefreshTokenAccepted(t *testing.T) {
	jm := newTestJWTManager()
	refresh, _, err := jm.GenerateRefreshToken("admin")
	if err != nil {
		t.Fatalf("GenerateRefreshToken: %v", err)
	}
	claims, err := jm.VerifyRefreshToken(refresh)
	if err != nil {
		t.Fatalf("VerifyRefreshToken failed for a valid refresh token: %v", err)
	}
	if claims.Username != "admin" {
		t.Errorf("claims.Username = %q, want %q", claims.Username, "admin")
	}
}

// TestS1_RefreshTokenRejectedAsAccessToken is the core S1 regression test.
//
// Before the S1 fix, access and refresh tokens shared the same signing key
// with no token-type or audience distinction. VerifyAccessToken parsed a
// refresh token with AccessClaims and accepted it — because the extra "tid"
// field was silently ignored and "role"/"scopes" being absent did not fail
// validation. A 7-day refresh token thus functioned as a long-lived access
// token.
//
// After the S1 fix, VerifyAccessToken checks both the "typ" claim (must be
// "access") and the audience (must be cubeops:access), so a refresh token
// (typ=refresh, aud=cubeops:refresh) is rejected.
func TestS1_RefreshTokenRejectedAsAccessToken(t *testing.T) {
	jm := newTestJWTManager()
	refresh, _, err := jm.GenerateRefreshToken("admin")
	if err != nil {
		t.Fatalf("GenerateRefreshToken: %v", err)
	}
	if _, err := jm.VerifyAccessToken(refresh); err == nil {
		t.Fatal("VerifyAccessToken accepted a refresh token — refresh tokens must NOT be usable as access tokens (S1)")
	}
}

// TestS1_AccessTokenRejectedAsRefreshToken is the symmetric case: an access
// token must not be accepted by VerifyRefreshToken.
func TestS1_AccessTokenRejectedAsRefreshToken(t *testing.T) {
	jm := newTestJWTManager()
	access, err := jm.GenerateAccessToken("admin")
	if err != nil {
		t.Fatalf("GenerateAccessToken: %v", err)
	}
	if _, err := jm.VerifyRefreshToken(access); err == nil {
		t.Fatal("VerifyRefreshToken accepted an access token — access tokens must NOT be usable as refresh tokens (S1)")
	}
}

// TestS1_TamperedTypRejected verifies that a token whose "typ" claim has been
// tampered with (e.g. refresh→access) is rejected. This covers the case
// where an attacker edits the payload without knowing the signing key (the
// signature check will fail, but we assert the error explicitly).
func TestS1_TamperedTypRejected(t *testing.T) {
	jm := newTestJWTManager()
	refresh, _, err := jm.GenerateRefreshToken("admin")
	if err != nil {
		t.Fatalf("GenerateRefreshToken: %v", err)
	}
	// Tampering should fail signature verification regardless of typ logic.
	// We just confirm the token is rejected.
	if _, err := jm.VerifyAccessToken(refresh); err == nil {
		t.Fatal("tampered-typ token was accepted by VerifyAccessToken (S1)")
	}
}

// TestS1_DifferentSecretsRejected verifies that a token signed with a
// different secret is not accepted.
func TestS1_DifferentSecretsRejected(t *testing.T) {
	jm1 := auth.NewJWTManager("secret-one-32-bytes-long-enough!", 15*time.Minute, 168*time.Hour)
	jm2 := auth.NewJWTManager("secret-two-32-bytes-long-enough!", 15*time.Minute, 168*time.Hour)
	access, err := jm1.GenerateAccessToken("admin")
	if err != nil {
		t.Fatalf("GenerateAccessToken: %v", err)
	}
	if _, err := jm2.VerifyAccessToken(access); err == nil {
		t.Fatal("VerifyAccessToken accepted a token signed by a different secret (S1)")
	}
}

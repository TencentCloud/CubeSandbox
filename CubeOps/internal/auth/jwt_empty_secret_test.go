// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
)

func mintWithEmptySecret(t *testing.T, claims jwt.Claims) string {
	t.Helper()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(""))
	if err != nil {
		t.Fatalf("failed to mint a token with an empty key: %v", err)
	}
	return signed
}

func forgedAccessToken(t *testing.T) string {
	t.Helper()
	now := time.Now()
	return mintWithEmptySecret(t, auth.AccessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute)),
			IssuedAt:  jwt.NewNumericDate(now),
			Subject:   "admin",
			Audience:  jwt.ClaimStrings{"cubeops:access"},
		},
		Username: "admin",
		Role:     "admin",
		Scopes:   []string{},
		Typ:      "access",
	})
}

func forgedRefreshToken(t *testing.T) string {
	t.Helper()
	now := time.Now()
	return mintWithEmptySecret(t, auth.RefreshClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(168 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(now),
			Subject:   "admin",
			Audience:  jwt.ClaimStrings{"cubeops:refresh"},
		},
		Username: "admin",
		TokenID:  "forged-token-id",
		Typ:      "refresh",
	})
}

func TestEmptySecretRejectedOnGenerate(t *testing.T) {
	jm := auth.NewJWTManager("", 15*time.Minute, 168*time.Hour)

	if _, err := jm.GenerateAccessToken("admin"); err == nil {
		t.Fatal("GenerateAccessToken accepted an empty signing secret")
	}
	if _, _, err := jm.GenerateRefreshToken("admin"); err == nil {
		t.Fatal("GenerateRefreshToken accepted an empty signing secret")
	}
}

func TestForgedTokenSignedWithEmptySecretIsRejected(t *testing.T) {
	empty := auth.NewJWTManager("", 15*time.Minute, 168*time.Hour)

	if _, err := empty.VerifyAccessToken(forgedAccessToken(t)); err == nil {
		t.Fatal("VerifyAccessToken accepted an admin token forged with the empty signing key")
	}
	if _, err := empty.VerifyRefreshToken(forgedRefreshToken(t)); err == nil {
		t.Fatal("VerifyRefreshToken accepted a token forged with the empty signing key")
	}
}

func TestWhitespaceOnlySecretIsTreatedAsEmpty(t *testing.T) {
	for _, secret := range []string{" ", "\t", "\n", "   \t\n "} {
		jm := auth.NewJWTManager(secret, 15*time.Minute, 168*time.Hour)

		if _, err := jm.GenerateAccessToken("admin"); err == nil {
			t.Errorf("GenerateAccessToken accepted a whitespace-only secret %q", secret)
		}
		if _, err := jm.VerifyAccessToken(forgedAccessToken(t)); err == nil {
			t.Errorf("VerifyAccessToken accepted a forged token with a whitespace-only secret %q", secret)
		}
	}
}

func TestNonEmptySecretStillWorks(t *testing.T) {
	jm := auth.NewJWTManager("good-secret-32-bytes-long-enough!", 15*time.Minute, 168*time.Hour)

	access, err := jm.GenerateAccessToken("admin")
	if err != nil {
		t.Fatalf("GenerateAccessToken: %v", err)
	}
	claims, err := jm.VerifyAccessToken(access)
	if err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
	if claims.Username != "admin" {
		t.Fatalf("Username = %q, want admin", claims.Username)
	}
}

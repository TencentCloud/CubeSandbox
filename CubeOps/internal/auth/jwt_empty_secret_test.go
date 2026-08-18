// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
)

func TestEmptySecretRejectedOnGenerate(t *testing.T) {
	jm := auth.NewJWTManager("", 15*time.Minute, 168*time.Hour)

	if _, err := jm.GenerateAccessToken("admin"); err == nil {
		t.Fatal("GenerateAccessToken accepted an empty signing secret")
	}
	if _, _, err := jm.GenerateRefreshToken("admin"); err == nil {
		t.Fatal("GenerateRefreshToken accepted an empty signing secret")
	}
}

func TestEmptySecretRejectedOnVerify(t *testing.T) {
	signer := auth.NewJWTManager("forged-secret-32-bytes-long-ok!!", 15*time.Minute, 168*time.Hour)
	minted, err := signer.GenerateAccessToken("admin")
	if err != nil {
		t.Fatalf("failed to mint a token for the test: %v", err)
	}

	empty := auth.NewJWTManager("", 15*time.Minute, 168*time.Hour)
	if _, err := empty.VerifyAccessToken(minted); err == nil {
		t.Fatal("VerifyAccessToken accepted a token while configured with an empty secret")
	}
	if _, err := empty.VerifyRefreshToken(minted); err == nil {
		t.Fatal("VerifyRefreshToken accepted a token while configured with an empty secret")
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

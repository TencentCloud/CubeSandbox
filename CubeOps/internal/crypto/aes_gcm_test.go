// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package crypto

import (
	"encoding/base64"
	"strings"
	"testing"
)

const testKeyB64 = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

func installTestKey(t *testing.T) {
	t.Helper()
	masterKeyMu.Lock()
	masterKey = nil
	masterKeyMu.Unlock()
	if err := InstallMasterKey(testKeyB64); err != nil {
		t.Fatalf("InstallMasterKey: %v", err)
	}
}

func TestEncryptDecryptRoundtrip(t *testing.T) {
	installTestKey(t)

	encrypted, err := EncryptSecret("wecom-secret")
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	if !IsEncrypted(encrypted) {
		t.Fatalf("expected encrypted format, got %q", encrypted)
	}

	decrypted, err := DecryptSecret(encrypted)
	if err != nil {
		t.Fatalf("DecryptSecret: %v", err)
	}
	if decrypted != "wecom-secret" {
		t.Fatalf("expected wecom-secret, got %q", decrypted)
	}
}

func TestEncryptProducesEncV1Prefix(t *testing.T) {
	installTestKey(t)

	encrypted, err := EncryptSecret("test")
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	if !strings.HasPrefix(encrypted, "enc:v1:") {
		t.Fatalf("expected enc:v1: prefix, got %q", encrypted)
	}
}

func TestDecryptOrPassthrough(t *testing.T) {
	installTestKey(t)

	if got := DecryptOrPassthrough("legacy-secret"); got != "legacy-secret" {
		t.Fatalf("expected legacy-secret, got %q", got)
	}

	enc, _ := EncryptSecret("my-secret")
	if got := DecryptOrPassthrough(enc); got != "my-secret" {
		t.Fatalf("expected my-secret, got %q", got)
	}

	if got := DecryptOrPassthrough("enc:v1:not-base64!!!"); got != "" {
		t.Fatalf("expected empty for malformed, got %q", got)
	}
}

func TestInstallMasterKeyRejectsWrongLength(t *testing.T) {
	shortKey := base64.StdEncoding.EncodeToString([]byte("too-short"))
	if err := InstallMasterKey(shortKey); err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestPasswordHashVerification(t *testing.T) {
	hashed, err := HashPassword("correct horse")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if !VerifyPassword(hashed, "correct horse") {
		t.Fatal("expected password to verify")
	}
	if VerifyPassword(hashed, "wrong horse") {
		t.Fatal("expected password to NOT verify")
	}

	if !VerifyPassword("legacy-password", "legacy-password") {
		t.Fatal("expected legacy plaintext to verify")
	}
	if VerifyPassword("legacy-password", "wrong") {
		t.Fatal("expected legacy plaintext to NOT verify with wrong password")
	}
}

func TestGenerateMasterKeyB64(t *testing.T) {
	key, err := GenerateMasterKeyB64()
	if err != nil {
		t.Fatalf("GenerateMasterKeyB64: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		t.Fatalf("generated key is not valid base64: %v", err)
	}
	if len(decoded) != masterKeyLen {
		t.Fatalf("expected %d bytes, got %d", masterKeyLen, len(decoded))
	}
}

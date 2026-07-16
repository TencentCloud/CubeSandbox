// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package crypto provides AES-256-GCM encryption and bcrypt password hashing
// compatible with the existing Rust implementation in CubeAPI/src/crypto.rs.
//
// The encrypted format is: enc:v1:<base64(nonce||ciphertext||tag)>
// where nonce is 12 bytes, matching the Rust aes-gcm crate's output.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

const (
	encPrefix    = "enc:v1:"
	nonceLen     = 12
	masterKeyLen = 32
)

var (
	masterKeyMu sync.RWMutex
	masterKey   []byte
)

// GenerateMasterKeyB64 generates a fresh random master key encoded as base64.
// crypto/rand failures (e.g. exhausted entropy source) are returned to the
// caller rather than panicking, so the caller — which already has an error
// path — can decide whether to retry, abort startup, or escalate.
func GenerateMasterKeyB64() (string, error) {
	bytes := make([]byte, masterKeyLen)
	if _, err := io.ReadFull(rand.Reader, bytes); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return base64.StdEncoding.EncodeToString(bytes), nil
}

// InstallMasterKey installs the process-wide master key from its base64 representation.
// Idempotent: installing the same key again is a no-op. Installing a different key
// after one is already installed returns an error — the caller must restart the
// process to rotate the key, otherwise previously encrypted data becomes undecryptable.
func InstallMasterKey(b64 string) error {
	bytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return fmt.Errorf("master key is not valid base64: %w", err)
	}
	if len(bytes) != masterKeyLen {
		return fmt.Errorf("master key must be exactly %d bytes, got %d", masterKeyLen, len(bytes))
	}
	masterKeyMu.Lock()
	defer masterKeyMu.Unlock()
	if masterKey != nil {
		if !bytesEqual(masterKey, bytes) {
			return errors.New("master key already installed with a different value; restart the process to rotate")
		}
		return nil
	}
	masterKey = bytes
	return nil
}

func loadKey() ([]byte, error) {
	masterKeyMu.RLock()
	defer masterKeyMu.RUnlock()
	if masterKey == nil {
		return nil, errors.New("AgentHub master key is not initialized")
	}
	return masterKey, nil
}

// ResetMasterKeyForTest clears the process-wide master key. It is only
// intended for tests that need to spin up a fresh database with a different
// key; calling it in production code would make all previously encrypted
// secrets undecryptable. The function is safe to call concurrently.
func ResetMasterKeyForTest() {
	masterKeyMu.Lock()
	defer masterKeyMu.Unlock()
	masterKey = nil
}

// EncryptSecret encrypts a UTF-8 secret, returning an enc:v1: tagged, base64 payload.
func EncryptSecret(plaintext string) (string, error) {
	key, err := loadKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("cipher.NewGCM: %w", err)
	}
	nonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("rand nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	payload := make([]byte, 0, nonceLen+len(ciphertext))
	payload = append(payload, nonce...)
	payload = append(payload, ciphertext...)
	return encPrefix + base64.StdEncoding.EncodeToString(payload), nil
}

// DecryptSecret decrypts an enc:v1: payload produced by EncryptSecret.
func DecryptSecret(stored string) (string, error) {
	encoded := strings.TrimPrefix(stored, encPrefix)
	if encoded == stored {
		return "", errors.New("value is not encrypted")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	if len(raw) <= nonceLen {
		return "", errors.New("ciphertext too short")
	}
	nonceBytes := raw[:nonceLen]
	ciphertext := raw[nonceLen:]
	key, err := loadKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plaintext, err := gcm.Open(nil, nonceBytes, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt failed: %w", err)
	}
	return string(plaintext), nil
}

// IsEncrypted returns whether a stored value is in the encrypted envelope format.
func IsEncrypted(stored string) bool {
	return strings.HasPrefix(stored, encPrefix)
}

// DecryptOrPassthrough decrypts a stored secret for use.
// Values without the enc:v1: envelope are legacy plaintext and returned unchanged.
// Encrypted values that cannot be decrypted return an empty string and log
// a warning so the failure is observable in logs (and alertable) rather
// than silently propagating an empty value to downstream callers.
func DecryptOrPassthrough(stored string) string {
	if !IsEncrypted(stored) {
		return stored
	}
	plaintext, err := DecryptSecret(stored)
	if err != nil {
		// Log only a prefix to avoid leaking the full ciphertext into logs.
		prefix := stored
		if len(prefix) > 16 {
			prefix = prefix[:16] + "..."
		}
		slog.Warn("DecryptOrPassthrough: decrypt failed; returning empty string",
			"err", err,
			"stored_prefix", prefix,
		)
		return ""
	}
	return plaintext
}

// HashPassword hashes a password with bcrypt.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash failed: %w", err)
	}
	return string(hash), nil
}

// VerifyPassword verifies a candidate password against a stored value.
// bcrypt hashes start with $2; anything else is treated as legacy plaintext.
func VerifyPassword(stored, candidate string) bool {
	if strings.HasPrefix(stored, "$2") {
		return bcrypt.CompareHashAndPassword([]byte(stored), []byte(candidate)) == nil
	}
	// Legacy plaintext path: compare in constant time to prevent an attacker
	// from inferring the stored value through response-time differences.
	return subtle.ConstantTimeCompare([]byte(stored), []byte(candidate)) == 1
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	// Master-key install already rejects length mismatches; use constant-time
	// compare for the actual byte check to avoid leaking key bytes via timing.
	return subtle.ConstantTimeCompare(a, b) == 1
}

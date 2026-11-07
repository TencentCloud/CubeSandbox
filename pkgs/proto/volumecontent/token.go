// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package volumecontent defines the short-lived credential that authorises
// access to a single volume through the /volumecontent data plane.
//
// The shape follows E2B's volume-content token: a JWT signed by the control
// plane, scoped to one volume, carrying an audience that must match the
// verifying service's own origin. Cube signs with HS256 and a shared secret
// (environment only), and additionally seals the plugin's opaque private data
// so the data plane can attach the volume without calling back into the
// control plane and without ever exposing that state to SDK users.
package volumecontent

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	EnvSecret   = "CUBE_VOLUME_CONTENT_TOKEN_SECRET"
	EnvIssuer   = "CUBE_VOLUME_CONTENT_TOKEN_ISSUER"
	EnvAudience = "CUBE_VOLUME_CONTENT_AUDIENCE"
	EnvTTL      = "CUBE_VOLUME_CONTENT_TOKEN_TTL"

	// PermRead allows the read-only routes; PermReadWrite also allows mutation.
	PermRead      = "r"
	PermReadWrite = "rw"

	DefaultIssuer = "cube-master"
	DefaultTTL    = time.Hour
	MinTTL        = time.Minute
	MaxTTL        = 24 * time.Hour
	MinSecretLen  = 32

	maxTokenLen = 8 * 1024
	clockSkew   = 30 * time.Second
)

var (
	// ErrInvalidToken is returned for every verification failure. Callers must
	// not distinguish causes to clients; details are for logs only.
	ErrInvalidToken = errors.New("invalid volume content token")
)

// Config is shared by the signer (CubeMaster) and the verifier (data plane).
type Config struct {
	secret   []byte
	Issuer   string
	Audience string
	TTL      time.Duration
}

// Claims is the verified content of a token.
type Claims struct {
	VolumeID    string
	Driver      string
	Perm        string
	PrivateData string
	ExpiresAt   time.Time
}

type wireClaims struct {
	Issuer    string `json:"iss"`
	Audience  string `json:"aud"`
	Expires   int64  `json:"exp"`
	IssuedAt  int64  `json:"iat"`
	NotBefore int64  `json:"nbf"`
	JTI       string `json:"jti"`
	VolumeID  string `json:"volid"`
	VolType   string `json:"voltype"`
	Perm      string `json:"perm"`
	Sealed    string `json:"pd,omitempty"`
}

// NewConfig validates and builds a Config.
func NewConfig(secret, issuer, audience string, ttl time.Duration) (*Config, error) {
	if len(secret) < MinSecretLen {
		return nil, fmt.Errorf("%s must be at least %d bytes", EnvSecret, MinSecretLen)
	}
	if strings.TrimSpace(audience) == "" {
		return nil, fmt.Errorf("%s is required when %s is set", EnvAudience, EnvSecret)
	}
	if issuer == "" {
		issuer = DefaultIssuer
	}
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl < MinTTL || ttl > MaxTTL {
		return nil, fmt.Errorf("%s must be between %s and %s", EnvTTL, MinTTL, MaxTTL)
	}
	return &Config{secret: []byte(secret), Issuer: issuer, Audience: audience, TTL: ttl}, nil
}

// ConfigFromEnv returns (nil, nil) when no secret is configured, meaning the
// feature is disabled. Any other misconfiguration is an error so callers fail
// closed instead of silently running without authentication.
func ConfigFromEnv(getenv func(string) string) (*Config, error) {
	secret := getenv(EnvSecret)
	if secret == "" {
		return nil, nil
	}
	var ttl time.Duration
	if raw := strings.TrimSpace(getenv(EnvTTL)); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvTTL, err)
		}
		ttl = parsed
	}
	return NewConfig(secret, strings.TrimSpace(getenv(EnvIssuer)), strings.TrimSpace(getenv(EnvAudience)), ttl)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (c *Config) mac(signingInput string) []byte {
	m := hmac.New(sha256.New, c.secret)
	m.Write([]byte(signingInput))
	return m.Sum(nil)
}

func (c *Config) aead() (cipher.AEAD, error) {
	k := hmac.New(sha256.New, c.secret)
	k.Write([]byte("cube-volume-content-private-data-v1"))
	block, err := aes.NewCipher(k.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (c *Config) seal(volumeID, privateData string) (string, error) {
	if privateData == "" {
		return "", nil
	}
	gcm, err := c.aead()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return b64(gcm.Seal(nonce, nonce, []byte(privateData), []byte(volumeID))), nil
}

func (c *Config) open(volumeID, sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		return "", err
	}
	gcm, err := c.aead()
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("sealed data too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte(volumeID))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// Mint signs a token for one volume.
func (c *Config) Mint(volumeID, driver, privateData, perm string, now time.Time) (string, error) {
	if volumeID == "" {
		return "", errors.New("volume id is required")
	}
	if perm != PermRead && perm != PermReadWrite {
		return "", fmt.Errorf("unsupported permission %q", perm)
	}
	sealed, err := c.seal(volumeID, privateData)
	if err != nil {
		return "", err
	}
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	claims, err := json.Marshal(wireClaims{
		Issuer: c.Issuer, Audience: c.Audience,
		Expires: now.Add(c.TTL).Unix(), IssuedAt: now.Unix(), NotBefore: now.Unix(),
		JTI: b64(jti), VolumeID: volumeID, VolType: driver, Perm: perm, Sealed: sealed,
	})
	if err != nil {
		return "", err
	}
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	signingInput := b64(header) + "." + b64(claims)
	return signingInput + "." + b64(c.mac(signingInput)), nil
}

// Verify checks signature, algorithm, issuer, audience and validity window.
// The caller must still compare Claims.VolumeID with the volume in the URL.
func (c *Config) Verify(token string, now time.Time) (*Claims, error) {
	if token == "" || len(token) > maxTokenLen {
		return nil, ErrInvalidToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrInvalidToken
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrInvalidToken
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil || header.Alg != "HS256" {
		return nil, ErrInvalidToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrInvalidToken
	}
	if !hmac.Equal(sig, c.mac(parts[0]+"."+parts[1])) {
		return nil, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrInvalidToken
	}
	var w wireClaims
	if err := json.Unmarshal(payload, &w); err != nil {
		return nil, ErrInvalidToken
	}
	if subtle.ConstantTimeCompare([]byte(w.Issuer), []byte(c.Issuer)) != 1 ||
		subtle.ConstantTimeCompare([]byte(w.Audience), []byte(c.Audience)) != 1 {
		return nil, ErrInvalidToken
	}
	if w.Expires == 0 || !now.Before(time.Unix(w.Expires, 0)) {
		return nil, ErrInvalidToken
	}
	if w.NotBefore != 0 && now.Add(clockSkew).Before(time.Unix(w.NotBefore, 0)) {
		return nil, ErrInvalidToken
	}
	if w.VolumeID == "" || (w.Perm != PermRead && w.Perm != PermReadWrite) {
		return nil, ErrInvalidToken
	}
	private, err := c.open(w.VolumeID, w.Sealed)
	if err != nil {
		return nil, ErrInvalidToken
	}
	return &Claims{
		VolumeID: w.VolumeID, Driver: w.VolType, Perm: w.Perm,
		PrivateData: private, ExpiresAt: time.Unix(w.Expires, 0),
	}, nil
}

// CanWrite reports whether the token permits mutating routes.
func (c *Claims) CanWrite() bool { return c.Perm == PermReadWrite }

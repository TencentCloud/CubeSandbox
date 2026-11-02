// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package volumecontent

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func testConfig(t *testing.T, audience string) *Config {
	t.Helper()
	cfg, err := NewConfig(testSecret, "", audience, 0)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestMintVerifyRoundTrip(t *testing.T) {
	cfg := testConfig(t, "https://vol.example.com")
	now := time.Now()
	tok, err := cfg.Mint("vol-1", "s3", "volumes/vol-1/", PermRead, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(tok, "volumes/vol-1") {
		t.Fatal("private data must not be readable from the token")
	}
	payload, _ := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[1])
	if strings.Contains(string(payload), "volumes/vol-1") {
		t.Fatal("private data leaked in claims")
	}
	got, err := cfg.Verify(tok, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.VolumeID != "vol-1" || got.Driver != "s3" || got.PrivateData != "volumes/vol-1/" || got.CanWrite() {
		t.Fatalf("unexpected claims: %+v", got)
	}
}

func TestVerifyRejects(t *testing.T) {
	cfg := testConfig(t, "https://vol.example.com")
	now := time.Now()
	good, err := cfg.Mint("vol-1", "s3", "", PermReadWrite, now)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := NewConfig("another-secret-another-secret-0000", "", "https://vol.example.com", 0)
	wrongAud, _ := NewConfig(testSecret, "", "https://evil.example.com", 0)
	noneHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	parts := strings.Split(good, ".")

	cases := map[string]string{
		"empty":          "",
		"garbage":        "a.b.c",
		"alg none":       noneHeader + "." + parts[1] + ".",
		"tampered claim": parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"volid":"vol-2"}`)) + "." + parts[2],
		"wrong secret":   mustMint(t, other, now),
		"wrong audience": mustMint(t, wrongAud, now),
		"two parts":      parts[0] + "." + parts[1],
	}
	for name, tok := range cases {
		if _, err := cfg.Verify(tok, now); err == nil {
			t.Errorf("%s: token accepted", name)
		}
	}
	if _, err := cfg.Verify(good, now.Add(cfg.TTL+time.Second)); err == nil {
		t.Error("expired token accepted")
	}
	if _, err := cfg.Verify(good, now.Add(-time.Hour)); err == nil {
		t.Error("token before nbf accepted")
	}
}

func mustMint(t *testing.T, cfg *Config, now time.Time) string {
	t.Helper()
	tok, err := cfg.Mint("vol-1", "s3", "", PermRead, now)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestConfigFromEnv(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if cfg, err := ConfigFromEnv(get); cfg != nil || err != nil {
		t.Fatalf("disabled config: %v %v", cfg, err)
	}
	env[EnvSecret] = "short"
	if _, err := ConfigFromEnv(get); err == nil {
		t.Fatal("short secret accepted")
	}
	env[EnvSecret] = testSecret
	if _, err := ConfigFromEnv(get); err == nil {
		t.Fatal("missing audience accepted")
	}
	env[EnvAudience] = "https://vol.example.com"
	env[EnvTTL] = "48h"
	if _, err := ConfigFromEnv(get); err == nil {
		t.Fatal("ttl above maximum accepted")
	}
	env[EnvTTL] = "30m"
	cfg, err := ConfigFromEnv(get)
	if err != nil || cfg.TTL != 30*time.Minute || cfg.Issuer != DefaultIssuer {
		t.Fatalf("valid config rejected: %+v %v", cfg, err)
	}
}

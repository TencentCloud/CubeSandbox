// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeOpenclawConfig writes a minimal openclaw.json with the given gateway
// token into dir (creating it if needed), so host-file reads resolve.
func writeOpenclawConfig(t *testing.T, dir, token string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	cfg := map[string]interface{}{
		"gateway": map[string]interface{}{
			"auth": map[string]interface{}{"token": token},
		},
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "openclaw.json"), data, 0o644); err != nil {
		t.Fatalf("write openclaw.json: %v", err)
	}
}

// writeOpenclawLastGood writes an openclaw.json.last-good file with the given
// token into dir.
func writeOpenclawLastGood(t *testing.T, dir, token string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	cfg := map[string]interface{}{
		"gateway": map[string]interface{}{
			"auth": map[string]interface{}{"token": token},
		},
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "openclaw.json.last-good"), data, 0o644); err != nil {
		t.Fatalf("write openclaw.json.last-good: %v", err)
	}
}

// TestResolveLastGoodToken_HostStable verifies the host-side .last-good token is
// returned once it reads the same value across consecutive samples.
func TestResolveLastGoodToken_HostStable(t *testing.T) {
	dir := t.TempDir()
	writeOpenclawLastGood(t, dir, "last-good-token")

	got := resolveOpenclawLastGoodToken(nil, "sb", "cube.app", dir, 5*time.Second)
	if got != "last-good-token" {
		t.Fatalf("resolveOpenclawLastGoodToken = %q, want %q", got, "last-good-token")
	}
}

// TestResolveLastGoodToken_Missing verifies an empty state dir yields "" and the
// polling eventually gives up (no hang).
func TestResolveLastGoodToken_Missing(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	got := resolveOpenclawLastGoodToken(nil, "sb", "cube.app", dir, 2*time.Second)
	if got != "" {
		t.Fatalf("resolveOpenclawLastGoodToken = %q, want empty", got)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("resolveOpenclawLastGoodToken did not give up within timeout")
	}
}

// TestResolveGatewayToken_PrefersLastGood verifies that ResolveGatewayToken
// persists the .last-good token (the value the live gateway actually enforces)
// even when openclaw.json holds a different value that is NOT enforced.
func TestResolveGatewayToken_PrefersLastGood(t *testing.T) {
	dir := t.TempDir()
	writeOpenclawConfig(t, dir, "not-enforced-file-token")
	writeOpenclawLastGood(t, dir, "enforced-last-good-token")

	got := ResolveGatewayToken(nil, "sb-host-full", "cube.app", dir, "generated-by-cubeops")
	if got != "enforced-last-good-token" {
		t.Fatalf("ResolveGatewayToken = %q, want %q", got, "enforced-last-good-token")
	}
}

// TestResolveLastGoodToken_MissingEnvdFast verifies that the envd path gives up
// immediately when .last-good is absent, instead of entering a long polling
// loop that spawns repeated interpreter processes.
func TestResolveLastGoodToken_MissingEnvdFast(t *testing.T) {
	start := time.Now()
	got := resolveOpenclawLastGoodToken(nil, "sb", "cube.app", "", 5*time.Second)
	if got != "" {
		t.Fatalf("resolveOpenclawLastGoodToken = %q, want empty", got)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("resolveOpenclawLastGoodToken(envd) did not fail-fast, took %s", time.Since(start))
	}
}

// TestResolveGatewayToken_PrefersFallbackWhenLastGoodMissing verifies that when
// no stable .last-good token is available, the CubeOps-generated fallback token
// is preferred over the (possibly transient) openclaw.json value.
func TestResolveGatewayToken_PrefersFallbackWhenLastGoodMissing(t *testing.T) {
	dir := t.TempDir()
	writeOpenclawConfig(t, dir, "transient-file-token")

	got := ResolveGatewayToken(nil, "sb-host-full", "cube.app", dir, "generated-by-cubeops")
	if got != "generated-by-cubeops" {
		t.Fatalf("ResolveGatewayToken = %q, want %q", got, "generated-by-cubeops")
	}
}

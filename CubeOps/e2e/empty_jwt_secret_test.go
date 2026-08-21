//go:build e2e

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// forgeAccessToken mints the token an attacker would build once they guess the
// signing secret is empty: sub=admin, aud=cubeops:access, typ=access, signed
// with a zero-length HMAC key. It deliberately does not go through JWTManager,
// which now refuses to sign with an empty key.
func forgeAccessToken(secret string) string {
	seg := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	now := time.Now().Unix()
	header := seg(map[string]any{"alg": "HS256", "typ": "JWT"})
	claims := seg(map[string]any{
		"sub": "admin", "username": "admin", "role": "admin", "scopes": []string{},
		"typ": "access", "aud": []string{"cubeops:access"},
		"iat": now, "exp": now + 900,
	})
	signingInput := header + "." + claims
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func readJWTSecret(t *testing.T, e *env) string {
	t.Helper()
	db := e.db(t)
	defer db.Close()
	var v *string
	err := db.QueryRow(
		"SELECT setting_value FROM t_system_setting WHERE setting_key = 'jwt_secret'").Scan(&v)
	if err != nil {
		t.Fatalf("read jwt_secret: %v", err)
	}
	if v == nil {
		return "<null>"
	}
	return *v
}

func setJWTSecret(t *testing.T, e *env, value any) {
	t.Helper()
	db := e.db(t)
	defer db.Close()
	if _, err := db.Exec(
		"REPLACE INTO t_system_setting (setting_key, setting_value) VALUES ('jwt_secret', ?)",
		value); err != nil {
		t.Fatalf("seed jwt_secret: %v", err)
	}
}

// TestEmptyJWTSecretEndToEnd is the wire-level guard for #1373. It runs the real
// binary against a real database and presents a forged admin token to a real
// authenticated route.
func TestEmptyJWTSecretEndToEnd(t *testing.T) {
	e := newEnv(t)
	defer e.teardown()

	// First boot runs migrations and seeds admin/admin.
	first := e.start(t)
	first.stop()

	for _, stored := range []any{"", " ", "\t", nil} {
		label := fmt.Sprintf("%#v", stored)
		t.Run("degenerate stored secret "+label, func(t *testing.T) {
			setJWTSecret(t, e, stored)

			inst := e.start(t)
			defer inst.stop()

			repaired := readJWTSecret(t, e)
			if strings.TrimSpace(repaired) == "" || repaired == "<null>" {
				t.Fatalf("jwt_secret is still degenerate after startup: %q", repaired)
			}

			forged := forgeAccessToken("")
			code, body := do(t, http.MethodGet, inst.baseURL+"/api/v1/auth/session", forged, "")
			if code == http.StatusOK {
				t.Fatalf("a token signed with the empty HMAC key was accepted: %s", body)
			}

			token := login(t, inst.baseURL, "admin", "admin")
			code, body = do(t, http.MethodGet, inst.baseURL+"/api/v1/auth/session", token, "")
			if code != http.StatusOK {
				t.Fatalf("a genuine token was rejected: %d %s", code, body)
			}
		})
	}
}

// TestBlankEnvJWTSecretIsNotUsedEndToEnd proves a whitespace-only JWT_SECRET no
// longer produces a server that boots but rejects every token it issues.
func TestBlankEnvJWTSecretIsNotUsedEndToEnd(t *testing.T) {
	e := newEnv(t)
	defer e.teardown()

	first := e.start(t)
	first.stop()

	inst := e.start(t, "JWT_SECRET=   ")
	defer inst.stop()

	token := login(t, inst.baseURL, "admin", "admin")
	code, body := do(t, http.MethodGet, inst.baseURL+"/api/v1/auth/session", token, "")
	if code != http.StatusOK {
		t.Fatalf("a blank JWT_SECRET left the server unable to honour its own tokens: %d %s", code, body)
	}

	forged := forgeAccessToken(" ")
	code, body = do(t, http.MethodGet, inst.baseURL+"/api/v1/auth/session", forged, "")
	if code == http.StatusOK {
		t.Fatalf("a token signed with the whitespace key was accepted: %s", body)
	}
}

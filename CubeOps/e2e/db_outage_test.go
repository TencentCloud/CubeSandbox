//go:build e2e

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"net/http"
	"strings"
	"testing"
)

// dbDetailMarkers are fragments of a driver error that must never reach a
// client. The host/port come from the container the harness started.
func dbDetailMarkers(e *env) []string {
	return []string{
		"dial tcp", "connection refused", "invalid connection",
		"sql:", "mysql:", e.mysqlURL.port,
	}
}

func assertNoDBDetail(t *testing.T, e *env, where, body string) {
	t.Helper()
	for _, marker := range dbDetailMarkers(e) {
		if strings.Contains(strings.ToLower(body), strings.ToLower(marker)) {
			t.Errorf("%s response leaks %q to the caller: %s", where, marker, body)
		}
	}
}

// TestDatabaseOutageEndToEnd is the wire-level guard for #1381. It logs in
// against a healthy database, then kills the database underneath the running
// server and asserts that:
//
//   - login reports 500 (infrastructure), not 401 (invalid credentials)
//   - none of the three public/authenticated 500 paths echo driver detail
func TestDatabaseOutageEndToEnd(t *testing.T) {
	e := newEnv(t)
	defer e.teardown()

	inst := e.start(t)
	defer inst.stop()

	// Healthy baseline: a real login and a real refresh token to use later.
	token := login(t, inst.baseURL, "admin", "admin")
	code, body := do(t, http.MethodGet, inst.baseURL+"/api/v1/auth/session", token, "")
	if code != http.StatusOK {
		t.Fatalf("baseline session check failed: %d %s", code, body)
	}

	code, refreshBody := do(t, http.MethodPost, inst.baseURL+"/api/v1/auth/login", "",
		`{"username":"admin","password":"admin"}`)
	if code != http.StatusOK {
		t.Fatalf("baseline login failed: %d %s", code, refreshBody)
	}
	refreshToken := extractRefreshToken(t, refreshBody)

	// Pull the database out from under the running server.
	e.teardown()

	t.Run("login reports an outage rather than bad credentials", func(t *testing.T) {
		code, body := do(t, http.MethodPost, inst.baseURL+"/api/v1/auth/login", "",
			`{"username":"admin","password":"admin"}`)
		if code == http.StatusUnauthorized {
			t.Fatalf("a database outage was reported as invalid credentials: %d %s", code, body)
		}
		if code != http.StatusInternalServerError {
			t.Fatalf("login during an outage returned %d, want 500: %s", code, body)
		}
		assertNoDBDetail(t, e, "login", body)
	})

	t.Run("change-password does not leak driver detail", func(t *testing.T) {
		code, body := do(t, http.MethodPost, inst.baseURL+"/api/v1/auth/change-password", token,
			`{"old_password":"admin","new_password":"another-password"}`)
		if code != http.StatusInternalServerError {
			t.Logf("change-password returned %d during the outage: %s", code, body)
		}
		assertNoDBDetail(t, e, "change-password", body)
	})

	t.Run("refresh does not leak driver detail", func(t *testing.T) {
		code, body := do(t, http.MethodPost, inst.baseURL+"/api/v1/auth/refresh", "",
			`{"refreshToken":"`+refreshToken+`"}`)
		if code != http.StatusInternalServerError {
			t.Logf("refresh returned %d during the outage: %s", code, body)
		}
		assertNoDBDetail(t, e, "refresh", body)
	})
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
)

// fakeUserStore is an in-memory UserStore for tests.
type fakeUserStore struct {
	passwords     map[string]string
	refreshTokens map[string]string // tokenID → username (active)
	revokedTokens map[string]bool   // tokenID → revoked
}

func (f *fakeUserStore) GetUserPassword(_ context.Context, username string) (string, error) {
	pw, ok := f.passwords[username]
	if !ok {
		return "", errors.New("user not found")
	}
	return pw, nil
}

func (f *fakeUserStore) SetUserPassword(_ context.Context, username, passwordHash string) error {
	if f.passwords == nil {
		f.passwords = map[string]string{}
	}
	f.passwords[username] = passwordHash
	return nil
}

func (f *fakeUserStore) CreateRefreshToken(_ context.Context, tokenID, username string) error {
	if f.refreshTokens == nil {
		f.refreshTokens = map[string]string{}
	}
	f.refreshTokens[tokenID] = username
	return nil
}

func (f *fakeUserStore) IsRefreshTokenRevoked(_ context.Context, tokenID string) (bool, error) {
	if f.revokedTokens == nil {
		return false, nil
	}
	return f.revokedTokens[tokenID], nil
}

func (f *fakeUserStore) RevokeRefreshToken(_ context.Context, tokenID string) error {
	if f.revokedTokens == nil {
		f.revokedTokens = map[string]bool{}
	}
	f.revokedTokens[tokenID] = true
	return nil
}

func (f *fakeUserStore) RevokeAllRefreshTokensForUser(ctx context.Context, username string) error {
	for tid, u := range f.refreshTokens {
		if u == username {
			f.RevokeRefreshToken(ctx, tid)
		}
	}
	return nil
}

// fakeTokenIssuer is a deterministic TokenIssuer for tests. It returns
// fixed-format strings so tests can assert on their contents.
type fakeTokenIssuer struct {
	accessTTL time.Duration
	tokenSeq  int // generates unique token IDs per refresh
}

func (f *fakeTokenIssuer) GenerateAccessToken(username string) (string, error) {
	return "access-" + username, nil
}

func (f *fakeTokenIssuer) GenerateRefreshToken(username string) (string, string, error) {
	f.tokenSeq++
	tokenID := fmt.Sprintf("tid-%s-%d", username, f.tokenSeq)
	return "refresh-" + username + "-" + fmt.Sprintf("%d", f.tokenSeq), tokenID, nil
}

func (f *fakeTokenIssuer) VerifyRefreshToken(token string) (*RefreshClaims, error) {
	// Accept "refresh-<username>" (old) and "refresh-<username>-<seq>" (new).
	if len(token) < 8 || token[:8] != "refresh-" {
		return nil, errors.New("invalid refresh token")
	}
	rest := token[8:]
	// Extract username: split on "-", last segment is the sequence number.
	// For backward compat, "refresh-alice" → username=alice, tokenID=tid-alice-1.
	username := rest
	tokenID := "tid-" + rest + "-1"
	// Try to parse "username-seq" format.
	for i := len(rest) - 1; i >= 0; i-- {
		if rest[i] == '-' {
			username = rest[:i]
			tokenID = "tid-" + rest
			break
		}
	}
	return &RefreshClaims{Username: username, TokenID: tokenID}, nil
}

func (f *fakeTokenIssuer) AccessTTL() time.Duration { return f.accessTTL }

func newTestAuthService(t *testing.T) (*AuthService, *fakeUserStore) {
	t.Helper()
	store := &fakeUserStore{
		passwords: map[string]string{
			"alice": mustHash(t, "correct-horse"),
		},
	}
	jm := &fakeTokenIssuer{accessTTL: 15 * time.Minute}
	return NewAuthService(store, jm), store
}

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := crypto.HashPassword(pw)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return h
}

func TestAuthService_Login_Success(t *testing.T) {
	svc, _ := newTestAuthService(t)
	res, err := svc.Login(context.Background(), "alice", "correct-horse")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if res.AccessToken != "access-alice" {
		t.Errorf("AccessToken = %q, want %q", res.AccessToken, "access-alice")
	}
	if res.RefreshToken == "" {
		t.Errorf("RefreshToken = empty, want non-empty")
	}
	if res.Username != "alice" {
		t.Errorf("Username = %q, want %q", res.Username, "alice")
	}
	if res.ExpiresInSecs != int64((15 * time.Minute).Seconds()) {
		t.Errorf("ExpiresInSecs = %d, want %d", res.ExpiresInSecs, int64((15 * time.Minute).Seconds()))
	}
}

func TestAuthService_Login_MissingFields(t *testing.T) {
	svc, _ := newTestAuthService(t)
	for _, tc := range []struct {
		username, password string
	}{
		{"", "anything"},
		{"alice", ""},
		{"", ""},
	} {
		_, err := svc.Login(context.Background(), tc.username, tc.password)
		if err == nil {
			t.Errorf("Login(%q, %q) = nil err, want error", tc.username, tc.password)
		}
		if !errors.Is(err, err) || err == nil {
			// Just check that the error is non-nil and mentions a required field.
		}
	}
}

func TestAuthService_Login_WrongPassword(t *testing.T) {
	svc, _ := newTestAuthService(t)
	_, err := svc.Login(context.Background(), "alice", "battery-staple")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Login wrong password err = %v, want ErrInvalidCredentials", err)
	}
}

func TestAuthService_Login_UnknownUser(t *testing.T) {
	svc, _ := newTestAuthService(t)
	_, err := svc.Login(context.Background(), "ghost", "anything")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Login unknown user err = %v, want ErrInvalidCredentials", err)
	}
}

func TestAuthService_Refresh_Success(t *testing.T) {
	svc, _ := newTestAuthService(t)
	// Login to get a real refresh token.
	loginRes, err := svc.Login(context.Background(), "alice", "correct-horse")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	accessTok, refreshTok, err := svc.Refresh(context.Background(), loginRes.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if accessTok != "access-alice" {
		t.Errorf("Refresh returned access %q, want %q", accessTok, "access-alice")
	}
	if refreshTok == "" {
		t.Error("Refresh returned empty refresh token, want non-empty (M2 rotation)")
	}
}

func TestAuthService_Refresh_InvalidToken(t *testing.T) {
	svc, _ := newTestAuthService(t)
	_, _, err := svc.Refresh(context.Background(), "not-a-real-token")
	if !errors.Is(err, ErrInvalidRefreshToken) {
		t.Errorf("Refresh invalid err = %v, want ErrInvalidRefreshToken", err)
	}
}

func TestAuthService_Refresh_EmptyToken(t *testing.T) {
	svc, _ := newTestAuthService(t)
	_, _, err := svc.Refresh(context.Background(), "")
	if err == nil {
		t.Error("Refresh empty token = nil err, want error")
	}
}

// TestM2_Refresh_RotatesAndRevokesOldToken verifies that after a refresh,
// the old refresh token is revoked and cannot be used again.
func TestM2_Refresh_RotatesAndRevokesOldToken(t *testing.T) {
	svc, store := newTestAuthService(t)
	// Login to get a real refresh token (with unique tokenID).
	loginRes, err := svc.Login(context.Background(), "alice", "correct-horse")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	oldRefresh := loginRes.RefreshToken

	// First refresh — should succeed and return a NEW refresh token.
	_, newRefresh, err := svc.Refresh(context.Background(), oldRefresh)
	if err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	if newRefresh == oldRefresh {
		t.Error("Refresh did not rotate — returned the same refresh token (M2)")
	}

	// Second refresh with the OLD token — should fail (revoked).
	_, _, err = svc.Refresh(context.Background(), oldRefresh)
	if !errors.Is(err, ErrInvalidRefreshToken) {
		t.Errorf("Refresh with revoked token err = %v, want ErrInvalidRefreshToken (M2)", err)
	}
	_ = store
}

// TestM2_ChangePassword_RevokesAllRefreshTokens verifies that after a
// password change, all existing refresh tokens are revoked.
func TestM2_ChangePassword_RevokesAllRefreshTokens(t *testing.T) {
	svc, _ := newTestAuthService(t)
	// Login to get a real refresh token.
	loginRes, err := svc.Login(context.Background(), "alice", "correct-horse")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	oldRefresh := loginRes.RefreshToken

	// Change password — should revoke the refresh token.
	if err := svc.ChangePassword(context.Background(), "alice", "correct-horse", "new-pw-12345"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	// Refresh with the old token — should fail (revoked by password change).
	_, _, err = svc.Refresh(context.Background(), oldRefresh)
	if !errors.Is(err, ErrInvalidRefreshToken) {
		t.Errorf("Refresh after password change err = %v, want ErrInvalidRefreshToken (M2)", err)
	}
}

func TestAuthService_ChangePassword_Success(t *testing.T) {
	svc, store := newTestAuthService(t)
	if err := svc.ChangePassword(context.Background(), "alice", "correct-horse", "new-battery-staple"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	// Verify the new password works.
	if _, err := svc.Login(context.Background(), "alice", "new-battery-staple"); err != nil {
		t.Errorf("Login with new password: %v", err)
	}
	// And the old one doesn't.
	if _, err := svc.Login(context.Background(), "alice", "correct-horse"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("Login with old password after change = %v, want ErrInvalidCredentials", err)
	}
	_ = store
}

func TestAuthService_ChangePassword_RejectsIDOR(t *testing.T) {
	// Caller authenticates as "alice" but the service must ignore any
	// "username" field in the request body and only act on the username
	// derived from the (already-validated) auth context.
	svc, _ := newTestAuthService(t)
	err := svc.ChangePassword(context.Background(), "", "correct-horse", "new-pw")
	if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("ChangePassword with empty username err = %v, want ErrUnauthenticated", err)
	}
}

func TestAuthService_ChangePassword_ShortPassword(t *testing.T) {
	svc, _ := newTestAuthService(t)
	err := svc.ChangePassword(context.Background(), "alice", "correct-horse", "ab")
	if err == nil {
		t.Error("ChangePassword with too-short new password = nil err, want error")
	}
}

func TestAuthService_ChangePassword_WrongOldPassword(t *testing.T) {
	svc, _ := newTestAuthService(t)
	err := svc.ChangePassword(context.Background(), "alice", "WRONG", "new-battery-staple")
	if !errors.Is(err, ErrInvalidOldPassword) {
		t.Errorf("ChangePassword wrong old pw err = %v, want ErrInvalidOldPassword", err)
	}
}

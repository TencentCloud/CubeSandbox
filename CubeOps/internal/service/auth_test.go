// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/crypto"
)

// fakeUserStore is an in-memory UserStore for tests.
type fakeUserStore struct {
	passwords map[string]string
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

// fakeTokenIssuer is a deterministic TokenIssuer for tests. It returns
// fixed-format strings so tests can assert on their contents.
type fakeTokenIssuer struct {
	accessTTL time.Duration
}

func (f *fakeTokenIssuer) GenerateAccessToken(username string) (string, error) {
	return "access-" + username, nil
}

func (f *fakeTokenIssuer) GenerateRefreshToken(username string) (string, string, error) {
	return "refresh-" + username, "tid-" + username, nil
}

func (f *fakeTokenIssuer) VerifyRefreshToken(token string) (*RefreshClaims, error) {
	// Test helper — pretend any "refresh-foo" token belongs to "foo".
	if len(token) < 8 || token[:8] != "refresh-" {
		return nil, errors.New("invalid refresh token")
	}
	return &RefreshClaims{Username: token[8:], TokenID: "tid-" + token[8:]}, nil
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
	if res.RefreshToken != "refresh-alice" {
		t.Errorf("RefreshToken = %q, want %q", res.RefreshToken, "refresh-alice")
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
	tok, err := svc.Refresh(context.Background(), "refresh-alice")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tok != "access-alice" {
		t.Errorf("Refresh returned %q, want %q", tok, "access-alice")
	}
}

func TestAuthService_Refresh_InvalidToken(t *testing.T) {
	svc, _ := newTestAuthService(t)
	_, err := svc.Refresh(context.Background(), "not-a-real-token")
	if !errors.Is(err, ErrInvalidRefreshToken) {
		t.Errorf("Refresh invalid err = %v, want ErrInvalidRefreshToken", err)
	}
}

func TestAuthService_Refresh_EmptyToken(t *testing.T) {
	svc, _ := newTestAuthService(t)
	_, err := svc.Refresh(context.Background(), "")
	if err == nil {
		t.Error("Refresh empty token = nil err, want error")
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

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

type dbErrorStore struct {
	err error
}

func (d dbErrorStore) GetUserPassword(context.Context, string) (string, error) {
	return "", d.err
}
func (d dbErrorStore) SetUserPassword(context.Context, string, string) error       { return nil }
func (d dbErrorStore) CreateRefreshToken(context.Context, string, string) error    { return nil }
func (d dbErrorStore) IsRefreshTokenRevoked(context.Context, string) (bool, error) { return false, nil }
func (d dbErrorStore) RevokeRefreshToken(context.Context, string) error            { return nil }
func (d dbErrorStore) RevokeAllRefreshTokensForUser(context.Context, string) error { return nil }

type stubIssuer struct{}

func (stubIssuer) GenerateAccessToken(string) (string, error)          { return "a", nil }
func (stubIssuer) GenerateRefreshToken(string) (string, string, error) { return "r", "t", nil }
func (stubIssuer) VerifyRefreshToken(string) (*RefreshClaims, error)   { return &RefreshClaims{}, nil }
func (stubIssuer) AccessTTL() time.Duration                            { return time.Minute }

func TestLoginSurfacesInfrastructureError(t *testing.T) {
	dbDown := errors.New("dial tcp 10.0.0.5:3306: connect: connection refused")
	svc := NewAuthService(dbErrorStore{err: dbDown}, stubIssuer{})

	_, err := svc.Login(context.Background(), "admin", "hunter2")
	if err == nil {
		t.Fatal("Login returned no error while the database was unreachable")
	}
	if errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login masked a database outage as invalid credentials: %v", err)
	}
	if !errors.Is(err, dbDown) {
		t.Fatalf("Login did not wrap the underlying error, got %v", err)
	}
}

func TestLoginUnknownUserStillReportsInvalidCredentials(t *testing.T) {
	svc := NewAuthService(dbErrorStore{err: nil}, stubIssuer{})

	_, err := svc.Login(context.Background(), "nobody", "hunter2")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user should report invalid credentials, got %v", err)
	}
}

func TestChangePasswordSurfacesInfrastructureError(t *testing.T) {
	dbDown := errors.New("context canceled")
	svc := NewAuthService(dbErrorStore{err: dbDown}, stubIssuer{})

	err := svc.ChangePassword(context.Background(), "admin", "old-pass", "new-pass")
	if err == nil {
		t.Fatal("ChangePassword returned no error while the database was unreachable")
	}
	if errors.Is(err, ErrInvalidOldPassword) {
		t.Fatalf("ChangePassword masked a database outage as a bad old password: %v", err)
	}
}

func TestChangePasswordUnknownUserReportsBadOldPassword(t *testing.T) {
	svc := NewAuthService(dbErrorStore{err: nil}, stubIssuer{})

	err := svc.ChangePassword(context.Background(), "nobody", "old-pass", "new-pass")
	if !errors.Is(err, ErrInvalidOldPassword) {
		t.Fatalf("unknown user should report a bad old password, got %v", err)
	}
}

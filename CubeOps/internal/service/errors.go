// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service

import "errors"

// Sentinel errors used by the service layer. HTTP handlers map these to
// status codes; tests assert on them without depending on HTTP details.
var (
	// ErrInvalidCredentials is returned on bad username/password.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrInvalidOldPassword is returned on change-password with wrong current password.
	ErrInvalidOldPassword = errors.New("current password is incorrect or user not found")
	// ErrInvalidRefreshToken is returned when a refresh token is invalid or expired.
	ErrInvalidRefreshToken = errors.New("invalid or expired refresh token")
	// ErrUnauthenticated is returned when a request requires an authenticated user.
	ErrUnauthenticated = errors.New("authentication required")
	// ErrNotFound is returned when a requested resource does not exist.
	ErrNotFound = errors.New("not found")
)

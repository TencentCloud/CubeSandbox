// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CreateRefreshToken persists a refresh token record as active (not revoked).
// Called by Login when issuing a new refresh token.
func (s *Store) CreateRefreshToken(ctx context.Context, tokenID, username string) error {
	result := s.db.WithContext(ctx).Exec(
		insertIgnorePrefix()+" INTO t_refresh_token (token_id, username) VALUES (?, ?)"+onConflictDoNothing(),
		tokenID, username,
	)
	if result.Error != nil {
		return fmt.Errorf("create refresh token: %w", result.Error)
	}
	return nil
}

// IsRefreshTokenRevoked reports whether a refresh token has been revoked.
// Returns (false, nil) if the token does not exist (treated as not revoked —
// the JWT signature/expiry check is the primary defense).
func (s *Store) IsRefreshTokenRevoked(ctx context.Context, tokenID string) (bool, error) {
	var revokedAt sql.NullString
	err := s.db.WithContext(ctx).Raw(
		"SELECT revoked_at FROM t_refresh_token WHERE token_id = ? LIMIT 1", tokenID,
	).Row().Scan(&revokedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil // token not tracked — rely on JWT validation
		}
		return false, fmt.Errorf("check refresh token: %w", err)
	}
	return revokedAt.Valid, nil
}

// RevokeRefreshToken marks a refresh token as revoked. Idempotent — revoking
// an already-revoked token is a no-op. Called by Refresh during rotation.
func (s *Store) RevokeRefreshToken(ctx context.Context, tokenID string) error {
	result := s.db.WithContext(ctx).Exec(
		"UPDATE t_refresh_token SET revoked_at = CURRENT_TIMESTAMP WHERE token_id = ? AND revoked_at IS NULL",
		tokenID,
	)
	if result.Error != nil {
		return fmt.Errorf("revoke refresh token: %w", result.Error)
	}
	return nil
}

// RevokeAllRefreshTokensForUser revokes all active refresh tokens for a user.
// Called by ChangePassword to invalidate existing sessions after password change.
func (s *Store) RevokeAllRefreshTokensForUser(ctx context.Context, username string) error {
	result := s.db.WithContext(ctx).Exec(
		"UPDATE t_refresh_token SET revoked_at = CURRENT_TIMESTAMP WHERE username = ? AND revoked_at IS NULL",
		username,
	)
	if result.Error != nil {
		return fmt.Errorf("revoke all refresh tokens: %w", result.Error)
	}
	return nil
}

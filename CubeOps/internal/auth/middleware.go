// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"net/http"
	"strings"
)

type contextKey string

const userContextKey contextKey = "user"

// Middleware returns an HTTP middleware that validates JWT access tokens.
// Requests without a valid token are rejected with 401.
// RBAC is reserved for future use; currently any valid token grants full access.
func Middleware(jm *JWTManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tokenStr := extractBearerToken(r)
			if tokenStr == "" {
				http.Error(w, `{"error":"missing or invalid Authorization header"}`, http.StatusUnauthorized)
				return
			}
			claims, err := jm.VerifyAccessToken(tokenStr)
			if err != nil {
				http.Error(w, `{"error":"invalid or expired token"}`, http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), userContextKey, claims.Username)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// UsernameFromContext extracts the authenticated username from the request context.
func UsernameFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(userContextKey).(string); ok {
		return v
	}
	return ""
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type contextKey string

const userContextKey contextKey = "user"

// Middleware returns a gin middleware that validates JWT access tokens.
// Requests without a valid token are rejected with 401.
//
// The validated username is stashed in two places so the rest of the code can
// read it either way:
//   - c.Set("username", ...)        — gin idiomatic
//   - context.WithValue(...)        — for non-gin callers / loggers
func Middleware(jm *JWTManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		tokenStr := extractBearerToken(c.GetHeader("Authorization"))
		if tokenStr == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid Authorization header"})
			return
		}
		claims, err := jm.VerifyAccessToken(tokenStr)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}
		// Replace the request context so downstream handlers can use
		// UsernameFromContext(r.Context()) or c.GetString("username").
		ctx := context.WithValue(c.Request.Context(), userContextKey, claims.Username)
		c.Request = c.Request.WithContext(ctx)
		c.Set("username", claims.Username)
		c.Next()
	}
}

// UsernameFromContext extracts the authenticated username from the request context.
func UsernameFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(userContextKey).(string); ok {
		return v
	}
	return ""
}

// extractBearerToken returns the bearer token from an Authorization header.
// Accepts "Bearer XXX" (case-insensitive on the scheme). Returns "" if the
// header is missing, malformed, or the scheme is not "Bearer".
func extractBearerToken(authzHeader string) string {
	if authzHeader == "" {
		return ""
	}
	parts := strings.SplitN(authzHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

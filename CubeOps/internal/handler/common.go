// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

// GetString returns the URL query parameter or the empty string.
func GetString(c *gin.Context, key string) string {
	return c.Query(key)
}

// GetInt returns the URL query parameter parsed as int, falling back to def
// on missing or malformed values.
func GetInt(c *gin.Context, key string, def int) int {
	v := c.Query(key)
	if v == "" {
		return def
	}
	n, err := parseInt(v)
	if err != nil {
		return def
	}
	return n
}

// GetInt64 returns the URL query parameter parsed as int64, falling back to
// def on missing or malformed values.
func GetInt64(c *gin.Context, key string, def int64) int64 {
	v := c.Query(key)
	if v == "" {
		return def
	}
	n, err := parseInt64(v)
	if err != nil {
		return def
	}
	return n
}

// parsePagination reads ?limit= and ?offset= query params and applies the
// store's default and max bounds:
//
//   - limit <= 0  → store.DefaultListLimit (50)
//   - limit > MaxListLimit → MaxListLimit (200), to prevent a single
//     request from pulling millions of rows
//   - offset < 0  → 0
//
// Malformed values fall back to the default (0) so the handler doesn't
// 400 on a typo; the client can always retry with valid params.
func parsePagination(c *gin.Context) (limit, offset int) {
	limit = GetInt(c, "limit", 0)
	if limit <= 0 {
		limit = store.DefaultListLimit
	}
	if limit > store.MaxListLimit {
		limit = store.MaxListLimit
	}
	offset = GetInt(c, "offset", 0)
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

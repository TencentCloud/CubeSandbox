// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"github.com/gin-gonic/gin"
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

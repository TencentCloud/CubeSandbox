// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// storeRefreshLimiter rate-limits POST /store/refresh. Each refresh
// triggers concurrent outbound HTTPS calls to external registries; an
// authenticated user could otherwise fan out arbitrarily many requests
// and exhaust the server's connection pool or trip upstream
// registry-side limits.
//
// The limiter is in-process and intentionally not shared across
// replicas — it bounds blast radius from a single misbehaving user,
// not global throughput. A window of 10s between refreshes is plenty
// for human UI use (the WebUI only auto-refreshes every 6h) and keeps
// upstream registry load low.
type storeRefreshLimiter struct {
	mu        sync.Mutex
	last      time.Time
	minPeriod time.Duration
}

var defaultStoreRefreshLimiter = &storeRefreshLimiter{
	minPeriod: 10 * time.Second,
}

// allow reports whether a refresh is allowed now and records the time
// when it is.
func (l *storeRefreshLimiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.last) < l.minPeriod {
		return false
	}
	l.last = now
	return true
}

// StoreRefreshRateLimit is a gin middleware that enforces a minimum
// interval between POST /store/refresh calls. It must be installed
// only on the /store/refresh route.
func StoreRefreshRateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !defaultStoreRefreshLimiter.allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "store refresh is rate limited, try again later",
			})
			return
		}
		c.Next()
	}
}

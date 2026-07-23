// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// loginLimiter is a per-IP sliding-window rate limiter for the login
// endpoint. It protects the weak default credentials (admin/admin) from
// brute-force attacks.
//
// Limit: maxAttempts failures per window per IP. Successful logins do not
// count. The limiter is in-process and conservative — it is intentionally
// not shared across replicas
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
	limit    int
	window   time.Duration
}

var defaultLoginLimiter = &loginLimiter{
	failures: make(map[string][]time.Time),
	limit:    5,               // 5 failed attempts
	window:   1 * time.Minute, // per minute per IP
}

// recordFailure records a failed login attempt for the given IP.
// It also prunes expired entries for this IP, and deletes the map entry
// entirely when the slice becomes empty — preventing unbounded memory
// growth from IPs that never return after their window expires.
func (l *loginLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-l.window)
	fails := l.failures[ip]
	// Drop expired entries.
	kept := fails[:0]
	for _, t := range fails {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	if len(kept) == 0 {
		delete(l.failures, ip)
	} else {
		l.failures[ip] = kept
	}
}

// isBlocked reports whether the IP has exceeded the failure limit.
// It also prunes expired entries and deletes the map entry when empty,
// so read-only checks also contribute to memory hygiene.
func (l *loginLimiter) isBlocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-l.window)
	fails := l.failures[ip]
	// Prune expired entries and count remaining.
	kept := fails[:0]
	count := 0
	for _, t := range fails {
		if t.After(cutoff) {
			kept = append(kept, t)
			count++
		}
	}
	if len(kept) == 0 {
		delete(l.failures, ip)
	} else {
		l.failures[ip] = kept
	}
	return count >= l.limit
}

// clientIP extracts the client IP from the request, honoring
// X-Forwarded-For (set by nginx). Falls back to RemoteAddr.
func clientIP(c *gin.Context) string {
	if xff := c.GetHeader("X-Forwarded-For"); xff != "" {
		// Use the first (leftmost) address — that is the original client.
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return xff[:i]
			}
		}
		return xff
	}
	return c.ClientIP()
}

// LoginRateLimit is a gin middleware that blocks IPs with too many recent
// failed login attempts. It must be installed only on the /auth/login route.
func LoginRateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := clientIP(c)
		if defaultLoginLimiter.isBlocked(ip) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "too many failed login attempts, try again later",
			})
			return
		}
		c.Next()
	}
}

// markLoginFailure is called by the Login handler when authentication fails.
// It is exported so the handler can trigger it after a failed login.
func markLoginFailure(c *gin.Context) {
	defaultLoginLimiter.recordFailure(clientIP(c))
}

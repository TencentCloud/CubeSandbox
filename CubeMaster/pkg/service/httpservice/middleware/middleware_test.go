// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
)

func TestGetCallerHostIP(t *testing.T) {
	t.Run("prefer explicit header", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set(constants.CallerHostIP, "10.0.0.8")

		assert.Equal(t, "10.0.0.8", getCallerHostIP(req))
	})

	t.Run("fallback to remote addr", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "10.0.0.9:23456"

		assert.Equal(t, "10.0.0.9", getCallerHostIP(req))
	})
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package telemetry

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var tracedRoutes = map[string]bool{
	"POST /cube/sandbox":             true,
	"POST /cube/template/from-image": true,
}

func GinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost || !tracedRoutes[c.Request.Method+" "+c.FullPath()] {
			c.Next()
			return
		}

		ctx := ExtractHTTP(c.Request.Context(), c.Request.Header)
		name := strings.TrimSpace(c.Request.Method + " " + c.FullPath())
		ctx, span := Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		c.Request = c.Request.WithContext(ctx)

		c.Next()

		status := c.Writer.Status()
		span.SetAttributes(attribute.Int("http.response.status_code", status))
		if status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, "")
		}
	}
}

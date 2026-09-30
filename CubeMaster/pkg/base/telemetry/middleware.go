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

// createRoute is the one route that is traced. It is spelled out here instead
// of imported from pkg/service/httpservice/cube because that package imports
// this one; keep it in step with cube.CubeURI()+cube.SandboxAction.
const createRoute = "/cube/sandbox"

// GinMiddleware traces sandbox create requests.
func GinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodPost || c.FullPath() != createRoute {
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

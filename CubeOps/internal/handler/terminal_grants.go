// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/auth"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
)

const maxTerminalGrantRequestBytes = 8 << 10

type TerminalGrantHandler struct {
	service terminalGrantService
}

type terminalGrantService interface {
	IssueTerminalGrant(context.Context, service.TerminalPrincipal, service.TerminalGrantRequest) (*service.TerminalGrantResponse, *service.TerminalError)
}

func NewTerminalGrantHandler(terminalService terminalGrantService) *TerminalGrantHandler {
	return &TerminalGrantHandler{service: terminalService}
}

func (h *TerminalGrantHandler) Register(r *gin.RouterGroup) {
	r.POST("/terminal/grants", h.Create)
}

func (h *TerminalGrantHandler) Create(c *gin.Context) {
	claims, ok := auth.AccessClaimsFromContext(c.Request.Context())
	if !ok {
		writeTerminalHTTPError(c, http.StatusUnauthorized, "UNAUTHORIZED")
		return
	}

	var request service.TerminalGrantRequest
	if err := decodeTerminalGrantRequest(c, &request); err != nil {
		slog.Warn("terminal grant rejected",
			"user_id", claims.Username,
			"reason", "PROTOCOL_ERROR",
		)
		writeTerminalHTTPError(c, http.StatusBadRequest, "PROTOCOL_ERROR")
		return
	}

	response, terminalErr := h.service.IssueTerminalGrant(c.Request.Context(), service.TerminalPrincipal{
		UserID: claims.Username,
		Role:   claims.Role,
	}, request)
	if terminalErr != nil {
		slog.Warn("terminal grant rejected",
			"user_id", claims.Username,
			"sandbox_id", request.SandboxID,
			"session_id", request.SessionID,
			"reason", terminalErr.Code,
		)
		writeTerminalHTTPError(c, terminalErr.Status, terminalErr.Code)
		return
	}
	c.JSON(http.StatusCreated, response)
}

func decodeTerminalGrantRequest(c *gin.Context, destination *service.TerminalGrantRequest) error {
	if c.Request.Body == nil {
		return errors.New("terminal grant request body is required")
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxTerminalGrantRequestBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func writeTerminalHTTPError(c *gin.Context, status int, code string) {
	// Keep the response deliberately small and stable. In particular, never
	// reflect a subprotocol, authorization header, token, or upstream body.
	c.JSON(status, gin.H{"error": code})
}

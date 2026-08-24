// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/httputil"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/service"
)

// WebhookHandler exposes webhook subscription CRUD, the deliveries query and
// the test delivery endpoint.
type WebhookHandler struct {
	svc            *service.WebhookService
	webhookEnabled bool
}

// NewWebhookHandler creates a WebhookHandler. webhookEnabled reflects the
// global webhook.enabled switch: when false, POST /:id/test returns 503
// because no worker is running to deliver the row.
func NewWebhookHandler(svc *service.WebhookService, webhookEnabled bool) *WebhookHandler {
	return &WebhookHandler{svc: svc, webhookEnabled: webhookEnabled}
}

// Register mounts the webhook routes on the given (authenticated) group.
func (h *WebhookHandler) Register(r *gin.RouterGroup) {
	r.POST("/webhooks", h.Create)
	r.GET("/webhooks", h.List)
	r.GET("/webhooks/:id", h.Get)
	r.PUT("/webhooks/:id", h.Update)
	r.DELETE("/webhooks/:id", h.Delete)
	r.POST("/webhooks/:id/test", h.Test)
	r.GET("/webhooks/:id/deliveries", h.ListDeliveries)
}

// Create handles POST /api/v1/webhooks.
func (h *WebhookHandler) Create(c *gin.Context) {
	var req service.WebhookCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	sub, svcErr := h.svc.Create(c.Request.Context(), &req)
	if svcErr != nil {
		writeWebhookServiceError(c, svcErr)
		return
	}
	httputil.WriteJSON(c, http.StatusCreated, sub)
}

// List handles GET /api/v1/webhooks.
func (h *WebhookHandler) List(c *gin.Context) {
	limit, offset := parseListParams(c)
	subs, svcErr := h.svc.List(c.Request.Context(), limit, offset)
	if svcErr != nil {
		writeWebhookServiceError(c, svcErr)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, subs)
}

// Get handles GET /api/v1/webhooks/:id.
func (h *WebhookHandler) Get(c *gin.Context) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	sub, svcErr := h.svc.Get(c.Request.Context(), id)
	if svcErr != nil {
		writeWebhookServiceError(c, svcErr)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, sub)
}

// Update handles PUT /api/v1/webhooks/:id.
func (h *WebhookHandler) Update(c *gin.Context) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	var req service.WebhookUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httputil.WriteError(c, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	sub, svcErr := h.svc.Update(c.Request.Context(), id, &req)
	if svcErr != nil {
		writeWebhookServiceError(c, svcErr)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, sub)
}

// Delete handles DELETE /api/v1/webhooks/:id (soft delete).
func (h *WebhookHandler) Delete(c *gin.Context) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	if svcErr := h.svc.Delete(c.Request.Context(), id); svcErr != nil {
		writeWebhookServiceError(c, svcErr)
		return
	}
	httputil.WriteNoContent(c)
}

// Test handles POST /api/v1/webhooks/:id/test. The global worker switch is
// checked here: disabled → 503, no delivery row is created.
func (h *WebhookHandler) Test(c *gin.Context) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	if !h.webhookEnabled {
		httputil.WriteError(c, http.StatusServiceUnavailable, "webhook delivery is disabled")
		return
	}
	d, svcErr := h.svc.CreateTestDelivery(c.Request.Context(), id)
	if svcErr != nil {
		writeWebhookServiceError(c, svcErr)
		return
	}
	httputil.WriteJSON(c, http.StatusCreated, gin.H{"delivery_id": d.ID})
}

// deliveryStatusPattern matches the delivery status enum used in query params.
var deliveryStatusPattern = regexp.MustCompile(`^[a-z_]+$`)

// eventIDPrefixPattern is the allowed charset for event_id_prefix.
var eventIDPrefixPattern = regexp.MustCompile(`^[a-zA-Z0-9_\-:.]+$`)

// ListDeliveries handles GET /api/v1/webhooks/:id/deliveries.
func (h *WebhookHandler) ListDeliveries(c *gin.Context) {
	id, ok := parseIDParam(c)
	if !ok {
		return
	}
	limit, offset := parseListParams(c)
	status := strings.TrimSpace(c.Query("status"))
	if status != "" && !deliveryStatusPattern.MatchString(status) {
		httputil.WriteError(c, http.StatusBadRequest, "invalid status")
		return
	}
	prefix := strings.TrimSpace(c.Query("event_id_prefix"))
	if prefix != "" && (len(prefix) > 128 || !eventIDPrefixPattern.MatchString(prefix)) {
		httputil.WriteError(c, http.StatusBadRequest, "invalid event_id_prefix")
		return
	}
	rows, svcErr := h.svc.ListDeliveries(c.Request.Context(), id, status, prefix, limit, offset)
	if svcErr != nil {
		writeWebhookServiceError(c, svcErr)
		return
	}
	httputil.WriteJSON(c, http.StatusOK, rows)
}

func parseIDParam(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httputil.WriteError(c, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

func parseListParams(c *gin.Context) (limit, offset int) {
	limit = 50
	if v := strings.TrimSpace(c.Query("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	// Cap at the store's hard limit (200); over-limit is truncated, not 400.
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset = 0
	if v := strings.TrimSpace(c.Query("offset")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	return limit, offset
}

func writeWebhookServiceError(c *gin.Context, svcErr *service.Error) {
	status := svcErr.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	if errors.Is(svcErr, service.ErrNotFound) {
		status = http.StatusNotFound
	}
	httputil.WriteError(c, status, svcErr.Message)
}

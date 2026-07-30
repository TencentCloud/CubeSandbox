// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

const maxInternalBatchEvents = 100

// Handler receives internal event batches from CubeAPI.
type Handler struct {
	ingress *Ingress
}

// NewHandler creates an internal webhook ingress handler.
func NewHandler(ingress *Ingress) *Handler {
	return &Handler{ingress: ingress}
}

// Register mounts the unauthenticated CubeAPI-to-CubeOps endpoint.
func (h *Handler) Register(routes gin.IRoutes) {
	routes.POST("/internal/webhook/events/batch", h.receiveBatch)
}

func (h *Handler) receiveBatch(c *gin.Context) {
	var batch InternalBatch
	decoder := json.NewDecoder(c.Request.Body)
	if err := decoder.Decode(&batch); err != nil {
		h.ingress.Stats().invalidBatches.Add(1)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON batch"})
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		h.ingress.Stats().invalidBatches.Add(1)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON batch"})
		return
	}
	if batch.SchemaVersion != internalSchemaVersion || len(batch.Events) == 0 {
		h.ingress.Stats().invalidBatches.Add(1)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid batch envelope"})
		return
	}
	if len(batch.Events) > maxInternalBatchEvents {
		h.ingress.Stats().invalidBatches.Add(1)
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "batch contains too many events"})
		return
	}
	for _, event := range batch.Events {
		if err := validateEvent(event); err != nil {
			h.ingress.Stats().invalidBatches.Add(1)
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid event"})
			return
		}
	}
	if !h.ingress.TryEnqueue(batch) {
		slog.Warn("webhook ingress rejected batch", "event_count", len(batch.Events), "queue_events", h.ingress.Queued())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhook ingress queue is full"})
		return
	}
	slog.Debug("webhook ingress accepted batch", "event_count", len(batch.Events), "queue_events", h.ingress.Queued())
	c.JSON(http.StatusAccepted, gin.H{"accepted": true})
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/lifecycle"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxstatus"
)

const sandboxStatusMaxItems = 1000

// RegisterInternalSandboxStatusRoutes mounts Cubelet restart-status reports
// outside HMAC. Cubelet cannot sign those routes. The sandbox network cannot
// reach this ClusterIP, so the handler does not check a token.
func RegisterInternalSandboxStatusRoutes(g *gin.RouterGroup) {
	g.POST("/internal/v1/sandbox-status:batch", handleSandboxStatusBatch)
}

type sandboxStatusReport struct {
	HostID string              `json:"host_id"`
	Items  []sandboxStatusItem `json:"items"`
}

type sandboxStatusItem struct {
	SandboxID               string     `json:"sandbox_id"`
	HostID                  string     `json:"host_id"`
	Phase                   string     `json:"phase"`
	RestartPolicy           string     `json:"restart_policy"`
	RestartState            string     `json:"restart_state"`
	RestartCount            int32      `json:"restart_count"`
	LastExitCode            *int32     `json:"last_exit_code"`
	LastExitReason          string     `json:"last_exit_reason"`
	LastRestartAt           *time.Time `json:"last_restart_at"`
	LastSuccessfulRestartAt *time.Time `json:"last_successful_restart_at"`
	LastFailedRestartAt     *time.Time `json:"last_failed_restart_at"`
	NextRestartAt           *time.Time `json:"next_restart_at"`
	StatusSeq               int64      `json:"status_seq"`
}

func handleSandboxStatusBatch(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	var body sandboxStatusReport
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status report"})
		return
	}
	if len(body.Items) == 0 || len(body.Items) > sandboxStatusMaxItems {
		c.JSON(http.StatusBadRequest, gin.H{"error": "items must contain 1 to 1000 entries"})
		return
	}

	ctx := c.Request.Context()
	accepted := 0
	failed := 0
	for i := range body.Items {
		item := body.Items[i]
		item.SandboxID = strings.TrimSpace(item.SandboxID)
		if item.SandboxID == "" || item.StatusSeq < 0 {
			failed++
			continue
		}
		if strings.TrimSpace(item.HostID) == "" {
			item.HostID = strings.TrimSpace(body.HostID)
		}
		rec := &models.SandboxStatus{
			SandboxID:               item.SandboxID,
			HostID:                  item.HostID,
			Phase:                   item.Phase,
			RestartPolicy:           item.RestartPolicy,
			RestartState:            item.RestartState,
			RestartCount:            item.RestartCount,
			LastExitCode:            item.LastExitCode,
			LastExitReason:          item.LastExitReason,
			LastRestartAt:           item.LastRestartAt,
			LastSuccessfulRestartAt: item.LastSuccessfulRestartAt,
			LastFailedRestartAt:     item.LastFailedRestartAt,
			NextRestartAt:           item.NextRestartAt,
			StatusSeq:               item.StatusSeq,
			ReportedAt:              time.Now(),
		}
		applied, prevState, err := sandboxstatus.Upsert(ctx, rec)
		if err != nil {
			failed++
			log.G(ctx).Warnf("sandbox status upsert %s: %v", item.SandboxID, err)
			continue
		}
		accepted++
		if applied && item.RestartState != prevState {
			if st, ok := lifecycleStateForRestart(item.RestartState); ok {
				lifecycle.PublishStateDefault(ctx, item.SandboxID, st, "cubelet")
			}
		}
	}
	// Any store error fails the whole batch. The node retries it; rows that
	// already landed are ignored as stale seq. A 200 would make the node
	// ack the failed ids and never send them again.
	if failed > 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"accepted": accepted, "failed": failed})
		return
	}
	c.JSON(http.StatusOK, gin.H{"accepted": accepted, "failed": failed})
}

func lifecycleStateForRestart(state string) (string, bool) {
	switch state {
	case "Restarting", "restarting":
		return lifecycle.StateRestarting, true
	case "BackOff", "backoff":
		return lifecycle.StateBackOff, true
	case "GaveUp", "gaveup":
		return lifecycle.StateGaveUp, true
	case "Running", "running":
		return lifecycle.StateRunning, true
	default:
		return "", false
	}
}

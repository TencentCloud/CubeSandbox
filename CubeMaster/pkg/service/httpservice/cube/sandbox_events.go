// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxevent"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// resolveSandboxIDFn is a seam for tests; resolution may scan the cluster.
var resolveSandboxIDFn = sandbox.ResolveSandboxID

// A short ID maps to the same full ID on every page of a listing, so the
// mapping is remembered briefly instead of being resolved for each page.
// The cache is bounded: when full, expired entries are dropped, or else the
// entry expiring soonest.
const (
	eventIDCacheTTL = 5 * time.Minute
	// A short ID that does not resolve is remembered briefly too, so polling it
	// does not scan the cluster on every request.
	eventIDMissTTL  = 30 * time.Second
	eventIDCacheMax = 1024
)

var (
	eventIDCacheMu sync.Mutex
	eventIDCache   = make(map[string]resolvedEventID, eventIDCacheMax)
)

type resolvedEventID struct {
	fullID  string
	expires time.Time
}

// Full IDs are 32 hex characters, so only a shorter hex string can be a prefix
// of one; anything else is never resolved (and never scans the cluster).
var shortIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{1,31}$`)

func resolveEventSandboxID(ctx context.Context, shortID string) string {
	shortID = strings.TrimSpace(shortID)
	if !shortIDPattern.MatchString(shortID) {
		return ""
	}
	now := time.Now()
	eventIDCacheMu.Lock()
	r, ok := eventIDCache[shortID]
	if ok && !now.Before(r.expires) {
		delete(eventIDCache, shortID)
		ok = false
	}
	eventIDCacheMu.Unlock()
	if ok {
		return r.fullID
	}
	fullID, err := resolveSandboxIDFn(ctx, shortID)
	ttl := eventIDCacheTTL
	if err != nil || fullID == "" {
		fullID, ttl = "", eventIDMissTTL
	}
	eventIDCacheMu.Lock()
	if len(eventIDCache) >= eventIDCacheMax {
		evictEventIDCacheLocked(now)
	}
	eventIDCache[shortID] = resolvedEventID{fullID: fullID, expires: now.Add(ttl)}
	eventIDCacheMu.Unlock()
	return fullID
}

// evictEventIDCacheLocked drops expired entries, or else the one expiring
// soonest, so a burst of new IDs cannot flush the remembered misses.
func evictEventIDCacheLocked(now time.Time) {
	var oldestKey string
	var oldest time.Time
	for key, entry := range eventIDCache {
		if !now.Before(entry.expires) {
			delete(eventIDCache, key)
			continue
		}
		if oldestKey == "" || entry.expires.Before(oldest) {
			oldestKey, oldest = key, entry.expires
		}
	}
	if len(eventIDCache) >= eventIDCacheMax && oldestKey != "" {
		delete(eventIDCache, oldestKey)
	}
}

// SandboxEventsReq is the request for GET/POST /cube/sandbox/events.
type SandboxEventsReq struct {
	types.Request
	SandboxID string `json:"sandboxID"`
	// Cursor is the ID of the last event already read; 0 starts at the beginning.
	Cursor uint64 `json:"cursor,omitempty"`
	// Limit is the maximum number of events to return (default 100, max 500).
	Limit int `json:"limit,omitempty"`
}

// SandboxLifecycleEvent is one lifecycle history entry.
type SandboxLifecycleEvent struct {
	ID         uint64 `json:"id"`
	Event      string `json:"event"`
	Timestamp  string `json:"timestamp"`
	TemplateID string `json:"templateID,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// SandboxEventsRes is the response for /cube/sandbox/events.
type SandboxEventsRes struct {
	*types.Res
	Events     []SandboxLifecycleEvent `json:"events"`
	NextCursor uint64                  `json:"nextCursor,omitempty"`
	HasMore    bool                    `json:"hasMore"`
}

// handleSandboxEventsAction lists a sandbox's lifecycle history oldest first.
// Events are stored under the full sandbox ID. The ID is first matched as
// given, which keeps the history of a deleted sandbox readable; only when that
// finds nothing, and the ID is not a full ID, is it resolved like the other
// read paths, so a short ID of a live sandbox works too. An unknown ID returns an empty list.
func handleSandboxEventsAction(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	setRetCode := func(code int64) {
		if rt != nil {
			rt.RetCode = code
		}
	}
	req := &SandboxEventsReq{}
	if err := utils.DecodeHttpBody(c.Request.Body, req); err != nil {
		req.SandboxID = c.Query("sandbox_id")
		if req.SandboxID == "" {
			req.SandboxID = c.Query("sandboxID")
		}
		if cursor := c.Query("cursor"); cursor != "" {
			req.Cursor, _ = strconv.ParseUint(cursor, 10, 64)
		}
		if l := c.Query("limit"); l != "" {
			req.Limit, _ = strconv.Atoi(l)
		}
	}

	if req.SandboxID == "" {
		setRetCode(int64(errorcode.ErrorCode_MasterParamsError))
		common.WriteAPI(c, &SandboxEventsRes{Res: &types.Res{Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_MasterParamsError),
			RetMsg:  "sandboxID is required",
		}}})
		return
	}

	ctx := c.Request.Context()
	rows, hasMore, err := sandboxevent.List(ctx, req.SandboxID, req.Cursor, req.Limit)
	// A full ID never needs resolving, and resolving a missing one would scan
	// every node on each poll; only short IDs fall back.
	if err == nil && len(rows) == 0 && !sandboxid.IsFullID(req.SandboxID) {
		if resolved := resolveEventSandboxID(ctx, req.SandboxID); resolved != "" && resolved != req.SandboxID {
			rows, hasMore, err = sandboxevent.List(ctx, resolved, req.Cursor, req.Limit)
		}
	}
	// With the history disabled (store not initialised) there is simply no
	// history, like an older CubeMaster without this endpoint.
	if errors.Is(err, sandboxevent.ErrNotReady) {
		rows, hasMore, err = nil, false, nil
	}
	if err != nil {
		CubeLog.Errorf("sandboxevent.List sandboxID=%s err=%v", req.SandboxID, err)
		setRetCode(int64(errorcode.ErrorCode_DBError))
		common.WriteAPI(c, &SandboxEventsRes{Res: &types.Res{Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_DBError),
			RetMsg:  "failed to list sandbox events",
		}}})
		return
	}

	events := make([]SandboxLifecycleEvent, 0, len(rows))
	for _, row := range rows {
		events = append(events, SandboxLifecycleEvent{
			ID:         row.ID,
			Event:      row.Event,
			Timestamp:  row.CreatedAt.UTC().Format(time.RFC3339Nano),
			TemplateID: row.TemplateID,
			Reason:     row.Reason,
		})
	}
	setRetCode(0)
	res := &SandboxEventsRes{
		Res:     &types.Res{Ret: &types.Ret{RetCode: 0, RetMsg: ""}},
		Events:  events,
		HasMore: hasMore,
	}
	if hasMore {
		res.NextCursor = rows[len(rows)-1].ID
	}
	common.WriteAPI(c, res)
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"context"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeTemplateCenter/pkg/image"
)

// pullProgressSink turns the high-frequency pull-progress callbacks emitted by
// the image package into Redis live snapshots, and pushes the durable terminal
// snapshot to CubeMaster through the status callback.
//
// Mirrors CubeMaster/pkg/templatecenter/job_pull_progress.go. The Redis key is
// the same (localcache.SetTemplateImageJobPullProgress), so a progress query
// served by ANY CubeMaster replica sees TC's live snapshot — this is what makes
// remote-mode progress reporting work without request affinity (design §9.4).
type pullProgressSink struct {
	ctx      context.Context
	jobID    string
	reporter *Reporter

	mu            sync.Mutex
	lastBytes     int64
	lastSpeedAt   time.Time
	lastSnap      image.PullProgress
	cacheTTLSet   bool
	lastHeartbeat time.Time
}

const pullProgressFlushTimeout = 5 * time.Second

// pullProgressHeartbeatInterval is how often a long pull also refreshes the
// job row's updated_at in the DB, not just the Redis live snapshot. Without
// this, a pull that runs longer than the reconciler's staleAfter (2h) leaves
// updated_at untouched and a HEALTHY build gets swept as FAILED mid-pull --
// after which the eventual BUILT callback flips FAILED back to BUILT. The
// interval must be comfortably below staleAfter.
const pullProgressHeartbeatInterval = 60 * time.Second

func newPullProgressSink(ctx context.Context, jobID string) *pullProgressSink {
	return &pullProgressSink{ctx: ctx, jobID: jobID}
}

// withReporter attaches a reporter so flush can persist the terminal snapshot
// through CubeMaster (TC never writes image_jobs directly).
func (s *pullProgressSink) withReporter(r *Reporter) *pullProgressSink {
	s.reporter = r
	return s
}

// onProgress is the image.ProgressFunc handed to PrepareSource. It is invoked
// from the streaming goroutines, so it must stay goroutine-safe.
func (s *pullProgressSink) onProgress(p image.PullProgress) {
	now := time.Now()

	s.mu.Lock()
	p.SpeedBPS = s.computeSpeedLocked(p.DownloadedBytes, now)
	s.lastSnap = p
	cacheWithTTL := !s.cacheTTLSet
	s.mu.Unlock()

	progress := &basetypes.TemplateImageJobPullProgressMap{
		JobID:               s.jobID,
		PullTotalBytes:      p.TotalBytes,
		PullDownloadedBytes: p.DownloadedBytes,
		PullTotalLayers:     int32(p.TotalLayers),
		PullCompletedLayers: int32(p.CompletedLayers),
		PullSpeedBPS:        p.SpeedBPS,
		UpdatedAtMs:         now.UnixMilli(),
	}

	cacheFn := localcache.SetTemplateImageJobPullProgressNoTTL
	if cacheWithTTL {
		cacheFn = localcache.SetTemplateImageJobPullProgress
	}
	if err := cacheFn(s.ctx, progress); err != nil {
		log.G(s.ctx).Debugf("cache pull progress for job %s failed: %v", s.jobID, err)
		return
	}
	if cacheWithTTL {
		s.mu.Lock()
		s.cacheTTLSet = true
		s.mu.Unlock()
	}

	// Heartbeat: periodically persist an intermediate progress update to the
	// DB so updated_at keeps moving during a long pull. This is what stops the
	// reconciler from sweeping a healthy multi-hour pull as FAILED. Throttled
	// to pullProgressHeartbeatInterval so a fast pull does not hammer the DB.
	s.mu.Lock()
	dueHeartbeat := s.reporter != nil && (s.lastHeartbeat.IsZero() || now.Sub(s.lastHeartbeat) >= pullProgressHeartbeatInterval)
	if dueHeartbeat {
		s.lastHeartbeat = now
	}
	s.mu.Unlock()
	if dueHeartbeat {
		s.flushProgress(p)
	}
}

// flushProgress persists an intermediate (non-terminal) progress update through
// CubeMaster. Best-effort: a transient callback failure must not fail the pull,
// and the next heartbeat retries. Unlike flush (terminal), this carries no
// status/phase transition semantics -- only refreshed progress + updated_at.
func (s *pullProgressSink) flushProgress(p image.PullProgress) {
	if s.reporter == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pullProgressFlushTimeout)
	defer cancel()
	values := map[string]any{
		"pull_total_bytes":      p.TotalBytes,
		"pull_downloaded_bytes": p.DownloadedBytes,
		"pull_total_layers":     p.TotalLayers,
		"pull_completed_layers": p.CompletedLayers,
		"pull_speed_bps":        p.SpeedBPS,
	}
	if err := s.reporter.Report(ctx, s.jobID, values); err != nil {
		log.G(s.ctx).Debugf("heartbeat progress report for job %s failed: %v", s.jobID, err)
	}
}

// flush pushes the latest snapshot to CubeMaster as the durable terminal value
// and drops the Redis live snapshot. completed must only be true when the pull
// actually succeeded, so failure paths keep partial numbers honest.
func (s *pullProgressSink) flush(completed bool) {
	s.mu.Lock()
	p := s.lastSnap
	s.mu.Unlock()

	if completed && p.TotalBytes > 0 {
		p.DownloadedBytes = p.TotalBytes
	}
	if completed && p.TotalLayers > 0 {
		p.CompletedLayers = p.TotalLayers
	}

	flushCtx, cancel := s.flushContext()
	defer cancel()

	if s.reporter != nil {
		if err := s.reporter.Report(flushCtx, s.jobID, map[string]any{
			"pull_total_bytes":      p.TotalBytes,
			"pull_downloaded_bytes": p.DownloadedBytes,
			"pull_total_layers":     p.TotalLayers,
			"pull_completed_layers": p.CompletedLayers,
			"pull_speed_bps":        int64(0),
		}); err != nil {
			log.G(s.ctx).Warnf("flush pull progress for job %s failed: %v", s.jobID, err)
		}
	}
	if err := localcache.DeleteTemplateImageJobPullProgress(flushCtx, s.jobID); err != nil {
		log.G(s.ctx).Warnf("delete pull progress cache for job %s failed: %v", s.jobID, err)
	}
}

func (s *pullProgressSink) flushContext() (context.Context, context.CancelFunc) {
	if s.ctx == nil {
		return context.WithTimeout(context.Background(), pullProgressFlushTimeout)
	}
	return context.WithTimeout(context.WithoutCancel(s.ctx), pullProgressFlushTimeout)
}

// computeSpeedLocked derives bytes/sec from the delta against the previous
// snapshot. The caller must hold s.mu.
func (s *pullProgressSink) computeSpeedLocked(downloaded int64, now time.Time) int64 {
	defer func() {
		s.lastBytes = downloaded
		s.lastSpeedAt = now
	}()
	if s.lastSpeedAt.IsZero() {
		return 0
	}
	dt := now.Sub(s.lastSpeedAt).Seconds()
	if dt <= 0 {
		return 0
	}
	delta := downloaded - s.lastBytes
	if delta <= 0 {
		return 0
	}
	return int64(float64(delta) / dt)
}

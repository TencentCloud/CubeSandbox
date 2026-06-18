// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/image"
)

var (
	cacheTemplateImageJobPullProgress       = localcache.SetTemplateImageJobPullProgress
	updateTemplateImageJobPullProgressCache = localcache.SetTemplateImageJobPullProgressNoTTL
	deleteTemplateImageJobPullProgress      = localcache.DeleteTemplateImageJobPullProgress
	updateTemplateImageJobPullProgress      = updateTemplateImageJob
)

const pullProgressFlushTimeout = 5 * time.Second

// jobPullProgressSink turns the high-frequency pull-progress callbacks emitted
// by the image package into Redis live snapshots, leaving MySQL for durable
// terminal snapshots. It also derives a smoothed download speed from successive
// snapshots.
type jobPullProgressSink struct {
	ctx   context.Context
	jobID string

	mu          sync.Mutex
	lastBytes   int64
	lastSpeedAt time.Time
	lastSnap    image.PullProgress
	cacheTTLSet bool
}

func newJobPullProgressSink(ctx context.Context, jobID string) *jobPullProgressSink {
	return &jobPullProgressSink{ctx: ctx, jobID: jobID}
}

// onProgress is the image.ProgressFunc handed to PrepareSource. It is invoked
// from the subprocess-streaming goroutines, so it is fully synchronised.
func (s *jobPullProgressSink) onProgress(p image.PullProgress) {
	now := time.Now()

	s.mu.Lock()
	speed := s.computeSpeedLocked(p.DownloadedBytes, now)
	p.SpeedBPS = speed
	s.lastSnap = p
	cacheWithTTL := !s.cacheTTLSet
	s.mu.Unlock()

	progress := pullProgressMap(s.jobID, p, now)
	cacheFn := updateTemplateImageJobPullProgressCache
	if cacheWithTTL {
		cacheFn = cacheTemplateImageJobPullProgress
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
}

// flush writes the latest pull-progress snapshot to MySQL unconditionally and
// removes the Redis live snapshot. completed should only be true after the pull
// command succeeds; failure paths use false so partial progress remains honest.
func (s *jobPullProgressSink) flush(completed bool) {
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

	if err := updateTemplateImageJobPullProgress(flushCtx, s.jobID, map[string]any{
		"pull_total_bytes":      p.TotalBytes,
		"pull_downloaded_bytes": p.DownloadedBytes,
		"pull_total_layers":     p.TotalLayers,
		"pull_completed_layers": p.CompletedLayers,
		"pull_speed_bps":        int64(0),
	}); err != nil {
		log.G(s.ctx).Warnf("flush pull progress for job %s failed: %v", s.jobID, err)
	}
	if err := deleteTemplateImageJobPullProgress(flushCtx, s.jobID); err != nil {
		log.G(s.ctx).Warnf("delete pull progress cache for job %s failed: %v", s.jobID, err)
	}
}

func (s *jobPullProgressSink) flushContext() (context.Context, context.CancelFunc) {
	if s.ctx == nil {
		return context.WithTimeout(context.Background(), pullProgressFlushTimeout)
	}
	return context.WithTimeout(context.WithoutCancel(s.ctx), pullProgressFlushTimeout)
}

// computeSpeedLocked derives bytes/sec from the delta against the previous
// snapshot. The caller must hold s.mu.
func (s *jobPullProgressSink) computeSpeedLocked(downloaded int64, now time.Time) int64 {
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

func pullProgressMap(jobID string, p image.PullProgress, now time.Time) *basetypes.TemplateImageJobPullProgressMap {
	return &basetypes.TemplateImageJobPullProgressMap{
		JobID:               jobID,
		PullTotalBytes:      p.TotalBytes,
		PullDownloadedBytes: p.DownloadedBytes,
		PullTotalLayers:     int32(p.TotalLayers),
		PullCompletedLayers: int32(p.CompletedLayers),
		PullSpeedBPS:        p.SpeedBPS,
		UpdatedAtMs:         now.UnixMilli(),
	}
}

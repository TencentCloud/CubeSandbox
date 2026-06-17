// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"testing"

	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/image"
)

func TestJobPullProgressSinkCachesLiveProgressOnly(t *testing.T) {
	restore := stubPullProgressIO(t)
	defer restore()

	var cached *basetypes.TemplateImageJobPullProgressMap
	cacheTemplateImageJobPullProgress = func(_ context.Context, p *basetypes.TemplateImageJobPullProgressMap) error {
		cached = p
		return nil
	}

	s := newJobPullProgressSink(context.Background(), "job-live")
	s.onProgress(image.PullProgress{
		TotalBytes:      100,
		DownloadedBytes: 40,
		TotalLayers:     5,
		CompletedLayers: 2,
		Percent:         40,
	})

	if cached == nil {
		t.Fatal("expected live progress to be cached")
	}
	if cached.JobID != "job-live" || cached.PullDownloadedBytes != 40 || cached.PullCompletedLayers != 2 {
		t.Fatalf("cached progress mismatch: %+v", cached)
	}
	if updatePullProgressCalls != 0 {
		t.Fatalf("onProgress must not write MySQL, got %d calls", updatePullProgressCalls)
	}
	if deletePullProgressCalls != 0 {
		t.Fatalf("onProgress must not delete Redis, got %d calls", deletePullProgressCalls)
	}
}

func TestJobPullProgressSinkFlushCompletedWritesFinalSnapshot(t *testing.T) {
	restore := stubPullProgressIO(t)
	defer restore()

	s := newJobPullProgressSink(context.Background(), "job-final")
	s.lastSnap = image.PullProgress{
		TotalBytes:      100,
		DownloadedBytes: 40,
		TotalLayers:     5,
		CompletedLayers: 2,
		SpeedBPS:        123,
	}
	s.flush(true)

	if updatePullProgressCalls != 1 {
		t.Fatalf("flush should write MySQL once, got %d", updatePullProgressCalls)
	}
	if got := lastPullProgressUpdate["pull_downloaded_bytes"]; got != int64(100) {
		t.Fatalf("downloaded bytes=%v want 100", got)
	}
	if got := lastPullProgressUpdate["pull_completed_layers"]; got != 5 {
		t.Fatalf("completed layers=%v want 5", got)
	}
	if got := lastPullProgressUpdate["pull_speed_bps"]; got != int64(0) {
		t.Fatalf("terminal speed=%v want 0", got)
	}
	if deletePullProgressCalls != 1 {
		t.Fatalf("flush should delete Redis once, got %d", deletePullProgressCalls)
	}
}

func TestJobPullProgressSinkFlushPartialKeepsPartialSnapshot(t *testing.T) {
	restore := stubPullProgressIO(t)
	defer restore()

	s := newJobPullProgressSink(context.Background(), "job-partial")
	s.lastSnap = image.PullProgress{
		TotalBytes:      100,
		DownloadedBytes: 40,
		TotalLayers:     5,
		CompletedLayers: 2,
	}
	s.flush(false)

	if got := lastPullProgressUpdate["pull_downloaded_bytes"]; got != int64(40) {
		t.Fatalf("partial downloaded bytes=%v want 40", got)
	}
	if got := lastPullProgressUpdate["pull_completed_layers"]; got != 2 {
		t.Fatalf("partial completed layers=%v want 2", got)
	}
}

func TestJobPullProgressSinkFlushIgnoresCanceledJobContext(t *testing.T) {
	restore := stubPullProgressIO(t)
	defer restore()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := newJobPullProgressSink(ctx, "job-canceled")
	s.lastSnap = image.PullProgress{
		TotalBytes:      100,
		DownloadedBytes: 40,
		TotalLayers:     5,
		CompletedLayers: 2,
	}
	s.flush(true)

	if updatePullProgressCalls != 1 {
		t.Fatalf("flush should write MySQL despite canceled job context, got %d calls", updatePullProgressCalls)
	}
	if deletePullProgressCalls != 1 {
		t.Fatalf("flush should delete Redis despite canceled job context, got %d calls", deletePullProgressCalls)
	}
	if updatePullProgressCtxErr != nil {
		t.Fatalf("update used canceled context: %v", updatePullProgressCtxErr)
	}
	if deletePullProgressCtxErr != nil {
		t.Fatalf("delete used canceled context: %v", deletePullProgressCtxErr)
	}
}

var (
	updatePullProgressCalls  int
	deletePullProgressCalls  int
	lastPullProgressUpdate   map[string]any
	updatePullProgressCtxErr error
	deletePullProgressCtxErr error
)

func stubPullProgressIO(t *testing.T) func() {
	t.Helper()
	origCache := cacheTemplateImageJobPullProgress
	origDelete := deleteTemplateImageJobPullProgress
	origUpdate := updateTemplateImageJobPullProgress
	updatePullProgressCalls = 0
	deletePullProgressCalls = 0
	lastPullProgressUpdate = nil
	updatePullProgressCtxErr = nil
	deletePullProgressCtxErr = nil
	cacheTemplateImageJobPullProgress = func(context.Context, *basetypes.TemplateImageJobPullProgressMap) error {
		return nil
	}
	deleteTemplateImageJobPullProgress = func(ctx context.Context, _ string) error {
		deletePullProgressCalls++
		deletePullProgressCtxErr = ctx.Err()
		return nil
	}
	updateTemplateImageJobPullProgress = func(ctx context.Context, _ string, values map[string]any) error {
		updatePullProgressCalls++
		updatePullProgressCtxErr = ctx.Err()
		lastPullProgressUpdate = values
		return nil
	}
	return func() {
		cacheTemplateImageJobPullProgress = origCache
		deleteTemplateImageJobPullProgress = origDelete
		updateTemplateImageJobPullProgress = origUpdate
	}
}

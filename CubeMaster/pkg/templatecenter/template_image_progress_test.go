// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"testing"

	basetypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestOverlayTemplateImageJobPullProgressUsesRedisForRunningJob(t *testing.T) {
	restore := stubGetPullProgress(t, &basetypes.TemplateImageJobPullProgressMap{
		JobID:               "job-running",
		PullTotalBytes:      100,
		PullDownloadedBytes: 60,
		PullTotalLayers:     5,
		PullCompletedLayers: 3,
		PullSpeedBPS:        20,
	}, true)
	defer restore()

	info := &types.TemplateImageJobInfo{
		JobID:               "job-running",
		Status:              JobStatusRunning,
		PullTotalBytes:      100,
		PullDownloadedBytes: 10,
		PullTotalLayers:     5,
		PullCompletedLayers: 1,
	}
	overlayTemplateImageJobPullProgress(context.Background(), info)

	if info.PullDownloadedBytes != 60 || info.PullCompletedLayers != 3 || info.PullSpeedBPS != 20 {
		t.Fatalf("live progress was not overlaid: %+v", info)
	}
}

func TestOverlayTemplateImageJobPullProgressSkipsTerminalJob(t *testing.T) {
	restore := stubGetPullProgress(t, &basetypes.TemplateImageJobPullProgressMap{
		JobID:               "job-ready",
		PullTotalBytes:      100,
		PullDownloadedBytes: 60,
		PullTotalLayers:     5,
		PullCompletedLayers: 3,
	}, true)
	defer restore()

	info := &types.TemplateImageJobInfo{
		JobID:               "job-ready",
		Status:              JobStatusReady,
		PullTotalBytes:      100,
		PullDownloadedBytes: 100,
		PullTotalLayers:     5,
		PullCompletedLayers: 5,
	}
	overlayTemplateImageJobPullProgress(context.Background(), info)

	if info.PullDownloadedBytes != 100 || info.PullCompletedLayers != 5 {
		t.Fatalf("terminal job should keep MySQL snapshot: %+v", info)
	}
}

func stubGetPullProgress(t *testing.T, progress *basetypes.TemplateImageJobPullProgressMap, ok bool) func() {
	t.Helper()
	orig := getTemplateImageJobPullProgress
	getTemplateImageJobPullProgress = func(context.Context, string) (*basetypes.TemplateImageJobPullProgressMap, bool) {
		return progress, ok
	}
	return func() {
		getTemplateImageJobPullProgress = orig
	}
}

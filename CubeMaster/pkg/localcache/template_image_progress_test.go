// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"testing"

	"github.com/gomodule/redigo/redis"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/types"
)

func TestTemplateImageJobPullProgressKey(t *testing.T) {
	if got, want := templateImageJobPullProgressKey("job-1"), "template_image_job_pull_progress:job-1"; got != want {
		t.Fatalf("key=%q want %q", got, want)
	}
}

func TestTemplateImageJobPullProgressRedisStructRoundTrip(t *testing.T) {
	values := []interface{}{
		[]byte("job_id"), []byte("job-1"),
		[]byte("pull_total_bytes"), []byte("100"),
		[]byte("pull_downloaded_bytes"), []byte("60"),
		[]byte("pull_total_layers"), []byte("5"),
		[]byte("pull_completed_layers"), []byte("3"),
		[]byte("pull_speed_bps"), []byte("20"),
		[]byte("updated_at_ms"), []byte("123456"),
	}
	out := &types.TemplateImageJobPullProgressMap{}
	if err := redis.ScanStruct(values, out); err != nil {
		t.Fatalf("ScanStruct: %v", err)
	}
	if out.JobID != "job-1" || out.PullTotalBytes != 100 || out.PullDownloadedBytes != 60 ||
		out.PullTotalLayers != 5 || out.PullCompletedLayers != 3 || out.PullSpeedBPS != 20 ||
		out.UpdatedAtMs != 123456 {
		t.Fatalf("scan mismatch: %+v", out)
	}
}

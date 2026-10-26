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

func TestTemplateImageJobPullProgressSetArgs(t *testing.T) {
	progress := &types.TemplateImageJobPullProgressMap{
		JobID:               "job-1",
		PullTotalBytes:      100,
		PullDownloadedBytes: 60,
		PullTotalLayers:     5,
		PullCompletedLayers: 3,
		PullSpeedBPS:        20,
		UpdatedAtMs:         123456,
	}
	args := templateImageJobPullProgressSetArgs("progress:key", progress)
	if len(args) < 4 {
		t.Fatalf("args too short: %v", args)
	}
	if args[0] != templateImageJobPullProgressSetScript {
		t.Fatalf("unexpected script arg")
	}
	if args[1] != 1 {
		t.Fatalf("numkeys=%v want 1", args[1])
	}
	if args[2] != "progress:key" {
		t.Fatalf("key=%v want progress:key", args[2])
	}
	if args[3] != templateImageJobPullProgressExpireSeconds {
		t.Fatalf("ttl=%v want %d", args[3], templateImageJobPullProgressExpireSeconds)
	}

	fields := map[string]interface{}{}
	for i := 4; i+1 < len(args); i += 2 {
		field, ok := args[i].(string)
		if !ok {
			t.Fatalf("field arg %d has type %T", i, args[i])
		}
		fields[field] = args[i+1]
	}
	if fields["job_id"] != "job-1" ||
		fields["pull_total_bytes"] != int64(100) ||
		fields["pull_downloaded_bytes"] != int64(60) ||
		fields["pull_total_layers"] != int32(5) ||
		fields["pull_completed_layers"] != int32(3) ||
		fields["pull_speed_bps"] != int64(20) ||
		fields["updated_at_ms"] != int64(123456) {
		t.Fatalf("unexpected fields: %+v", fields)
	}
}

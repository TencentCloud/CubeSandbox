// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package kernelcmdline

import (
	"fmt"
	"testing"

	jsoniter "github.com/json-iterator/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

func TestUnionJSON(t *testing.T) {
	t.Run("empty existing keeps new", func(t *testing.T) {
		got := UnionJSON("", `["damon_reclaim.enabled=Y"]`)
		assert.JSONEq(t, `["damon_reclaim.enabled=Y"]`, got)
	})

	t.Run("union of video and damon", func(t *testing.T) {
		video := `["video=vfb:enable,720x1280M-32@60","vfb.videomemorysize=4423680"]`
		damon := `["damon_reclaim.enabled=Y","damon_reclaim.monitor_region_start=4096"]`
		got := UnionJSON(video, damon)
		var params []string
		require.NoError(t, jsoniter.Unmarshal([]byte(got), &params))
		assert.ElementsMatch(t, []string{
			"video=vfb:enable,720x1280M-32@60",
			"vfb.videomemorysize=4423680",
			"damon_reclaim.enabled=Y",
			"damon_reclaim.monitor_region_start=4096",
		}, params)
	})

	t.Run("duplicate key keeps later value", func(t *testing.T) {
		got := UnionJSON(
			`["damon_reclaim.enabled=Y"]`,
			`["damon_reclaim.enabled=N"]`,
		)
		assert.JSONEq(t, `["damon_reclaim.enabled=N"]`, got)
	})

	t.Run("duplicate key inside first array keeps last value", func(t *testing.T) {
		got := UnionJSON(
			`["damon_reclaim.enabled=Y","damon_reclaim.enabled=N"]`,
			"",
		)
		assert.JSONEq(t, `["damon_reclaim.enabled=N"]`, got)
	})

	t.Run("blank entries dropped", func(t *testing.T) {
		got := UnionJSON(`[" "]`, `["damon_reclaim.enabled=Y"]`)
		assert.JSONEq(t, `["damon_reclaim.enabled=Y"]`, got)
	})
}

func TestMergeAnnotation(t *testing.T) {
	t.Run("non-kernel key overwrites", func(t *testing.T) {
		dst := map[string]string{"other": "old"}
		MergeAnnotation(dst, "other", "new")
		assert.Equal(t, "new", dst["other"])
	})

	t.Run("kernel key unions", func(t *testing.T) {
		dst := map[string]string{
			constants.AnnotationVMKernelCmdlineAppend: `["video=vfb:enable,720x1280M-32@60"]`,
		}
		MergeAnnotation(dst, constants.AnnotationVMKernelCmdlineAppend,
			`["damon_reclaim.enabled=Y"]`)
		var params []string
		require.NoError(t, jsoniter.Unmarshal(
			[]byte(dst[constants.AnnotationVMKernelCmdlineAppend]), &params))
		assert.ElementsMatch(t, []string{
			"video=vfb:enable,720x1280M-32@60",
			"damon_reclaim.enabled=Y",
		}, params)
	})

	t.Run("order independent for distinct keys", func(t *testing.T) {
		video := `["video=vfb:enable,720x1280M-32@60","vfb.videomemorysize=4423680"]`
		damon := `["damon_reclaim.enabled=Y"]`

		forward := map[string]string{}
		MergeAnnotation(forward, constants.AnnotationVMKernelCmdlineAppend, video)
		MergeAnnotation(forward, constants.AnnotationVMKernelCmdlineAppend, damon)

		reverse := map[string]string{}
		MergeAnnotation(reverse, constants.AnnotationVMKernelCmdlineAppend, damon)
		MergeAnnotation(reverse, constants.AnnotationVMKernelCmdlineAppend, video)

		var fwd, rev []string
		require.NoError(t, jsoniter.Unmarshal(
			[]byte(forward[constants.AnnotationVMKernelCmdlineAppend]), &fwd))
		require.NoError(t, jsoniter.Unmarshal(
			[]byte(reverse[constants.AnnotationVMKernelCmdlineAppend]), &rev))
		assert.ElementsMatch(t, fwd, rev)
	})
}

func TestDamonMonitorRegion(t *testing.T) {
	const gib = uint64(1) << 30
	cases := []struct {
		name      string
		arch      string
		mem       uint64
		wantStart uint64
		wantEnd   uint64
		wantOK    bool
	}{
		{"amd64 zero", "amd64", 0, 0, 0, false},
		// start is the first page (4096), so a guest with at most one page of
		// RAM has no valid region and must fail closed.
		{"amd64 1 byte", "amd64", 1, 0, 0, false},
		{"amd64 one page", "amd64", 4096, 0, 0, false},
		{"amd64 2G", "amd64", 2 * gib, 4096, 2 * gib, true},
		{"amd64 3G", "amd64", 3 * gib, 4096, 3 * gib, true},
		{"amd64 4G", "amd64", 4 * gib, 4096, 5 * gib, true},
		{"amd64 255G", "amd64", 255 * gib, 4096, 256 * gib, true},
		{"amd64 256G", "amd64", 256 * gib, 4096, 257 * gib, true},
		{"arm64 zero", "arm64", 0, 0, 0, false},
		{"arm64 1G", "arm64", 1 * gib, 1 * gib, 2 * gib, true},
		{"arm64 2G", "arm64", 2 * gib, 1 * gib, 3 * gib, true},
		// 0xbc00_0000 (2.9375 GiB) is the largest RAM size that fits below
		// MEM_32BIT_RESERVED_START; one byte more spills to 4 GiB.
		{"arm64 hole edge", "arm64", 0xbc00_0000, 1 * gib, 0xfc00_0000, true},
		{"arm64 hole +1", "arm64", 0xbc00_0000 + 1, 1 * gib, 0x1_0000_0000 + 1, true},
		{"arm64 4G", "arm64", 4 * gib, 1 * gib, 5*gib + gib/16, true},
		{"arm64 255G", "arm64", 255 * gib, 1 * gib, 256*gib + gib/16, true},
		{"unsupported arch", "s390x", 4 * gib, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end, ok := DamonMonitorRegion(tc.arch, tc.mem)
			assert.Equal(t, tc.wantOK, ok)
			if !tc.wantOK {
				return
			}
			assert.Equal(t, tc.wantStart, start)
			assert.Equal(t, tc.wantEnd, end)
		})
	}
}

func TestFillDamonMonitorRegion(t *testing.T) {
	const mib = uint64(1) << 20
	const amd64 = "amd64"

	t.Run("append when absent", func(t *testing.T) {
		out, err := FillDamonMonitorRegion(`["damon_reclaim.enabled=Y"]`, amd64, 4096*mib)
		require.NoError(t, err)
		var params []string
		require.NoError(t, jsoniter.Unmarshal([]byte(out), &params))
		assert.Equal(t, []string{
			"damon_reclaim.enabled=Y",
			"damon_reclaim.monitor_region_start=4096",
			"damon_reclaim.monitor_region_end=5368709120",
		}, params)
	})

	t.Run("keep explicit non-zero region", func(t *testing.T) {
		in := `["damon_reclaim.monitor_region_start=1048576","damon_reclaim.monitor_region_end=3221225471"]`
		out, err := FillDamonMonitorRegion(in, amd64, 4096*mib)
		require.NoError(t, err)
		assert.JSONEq(t, in, out)
	})

	t.Run("reject single-sided start", func(t *testing.T) {
		_, err := FillDamonMonitorRegion(
			`["damon_reclaim.enabled=Y","damon_reclaim.monitor_region_start=1048576"]`, amd64, 4096*mib)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must both be set")
	})

	t.Run("reject single-sided end", func(t *testing.T) {
		_, err := FillDamonMonitorRegion(
			`["damon_reclaim.enabled=Y","damon_reclaim.monitor_region_end=1024"]`, amd64, 4096*mib)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must both be set")
	})

	t.Run("reject start not before end", func(t *testing.T) {
		_, err := FillDamonMonitorRegion(
			`["damon_reclaim.monitor_region_start=4096","damon_reclaim.monitor_region_end=4096"]`,
			amd64, 4096*mib)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "start < end")
	})

	t.Run("reject unparseable explicit bound", func(t *testing.T) {
		_, err := FillDamonMonitorRegion(
			`["damon_reclaim.monitor_region_end=abc"]`, amd64, 4096*mib)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be a decimal or 0x-prefixed integer")
	})

	t.Run("replace explicit zero region", func(t *testing.T) {
		out, err := FillDamonMonitorRegion(
			`["damon_reclaim.monitor_region_start=0","damon_reclaim.monitor_region_end=0"]`, amd64, 2048*mib)
		require.NoError(t, err)
		var params []string
		require.NoError(t, jsoniter.Unmarshal([]byte(out), &params))
		assert.Equal(t, []string{
			"damon_reclaim.monitor_region_start=4096",
			"damon_reclaim.monitor_region_end=2147483648", // 2G <= 3G, end = mem
		}, params)
	})

	t.Run("fail closed when the region cannot be derived", func(t *testing.T) {
		// Zero RAM and zero placeholders: the region is unknown, so the caller
		// must supply both bounds instead of silently keeping 0/0.
		_, err := FillDamonMonitorRegion(
			`["damon_reclaim.monitor_region_start=0","damon_reclaim.monitor_region_end=0"]`, amd64, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot derive")
	})

	t.Run("explicit bounds survive an unknown arch", func(t *testing.T) {
		in := `["damon_reclaim.monitor_region_start=1048576","damon_reclaim.monitor_region_end=3221225471"]`
		out, err := FillDamonMonitorRegion(in, "s390x", 0)
		require.NoError(t, err)
		assert.JSONEq(t, in, out)
	})

	t.Run("treat 00 and 0x0 as zero placeholders", func(t *testing.T) {
		for _, v := range []string{"0", "00", "0x0"} {
			in := fmt.Sprintf(
				`["damon_reclaim.monitor_region_start=%s","damon_reclaim.monitor_region_end=%s"]`, v, v)
			out, err := FillDamonMonitorRegion(in, amd64, 4096*mib)
			require.NoError(t, err)
			var params []string
			require.NoError(t, jsoniter.Unmarshal([]byte(out), &params))
			assert.Equal(t, []string{
				"damon_reclaim.monitor_region_start=4096",
				"damon_reclaim.monitor_region_end=5368709120",
			}, params, "value %q", v)
		}
	})

	t.Run("reject empty bound", func(t *testing.T) {
		_, err := FillDamonMonitorRegion(
			`["damon_reclaim.monitor_region_start=","damon_reclaim.monitor_region_end="]`, amd64, 4096*mib)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be a decimal or 0x-prefixed integer")
	})

	t.Run("video params survive the fill", func(t *testing.T) {
		in := `["video=vfb:enable,720x1280M-32@60","vfb.videomemorysize=4423680","damon_reclaim.enabled=Y"]`
		out, err := FillDamonMonitorRegion(in, amd64, 4096*mib)
		require.NoError(t, err)
		var params []string
		require.NoError(t, jsoniter.Unmarshal([]byte(out), &params))
		assert.Contains(t, params, "video=vfb:enable,720x1280M-32@60")
		assert.Contains(t, params, "vfb.videomemorysize=4423680")
		assert.Contains(t, params, "damon_reclaim.monitor_region_start=4096")
		assert.Contains(t, params, "damon_reclaim.monitor_region_end=5368709120")
	})

	t.Run("no damon param leaves list untouched", func(t *testing.T) {
		in := `["page_reporting.page_reporting_order=6"]`
		out, err := FillDamonMonitorRegion(in, amd64, 4096*mib)
		require.NoError(t, err)
		assert.JSONEq(t, in, out)
	})

	t.Run("empty input", func(t *testing.T) {
		out, err := FillDamonMonitorRegion("", amd64, 4096*mib)
		require.NoError(t, err)
		assert.Empty(t, out)
	})
}

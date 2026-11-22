// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	goruntime "runtime"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	jsoniter "github.com/json-iterator/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/kernelcmdline"
	cgroupp "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
)

// applyDamonMonitorRegionOpt runs genDamonMonitorRegionOpt against s the same
// way containerd.WithSpec applies the sandbox spec options.
func applyDamonMonitorRegionOpt(flowOpts *workflow.CreateContext, s *oci.Spec) error {
	l := &local{}
	return l.genDamonMonitorRegionOpt(flowOpts)(context.Background(), nil, nil, s)
}

// TestGenDamonMonitorRegionOptMergesVideoAndDamon covers the full sandbox-spec
// path: video producers and the DAMON monitor region share the
// cube.vm.kernel.cmdline.append annotation, so the fill must preserve the
// video params while appending the derived region.
func TestGenDamonMonitorRegionOptMergesVideoAndDamon(t *testing.T) {
	const (
		mib    = uint64(1) << 20
		memMiB = 4096
	)
	wantStart, wantEnd, ok := kernelcmdline.DamonMonitorRegion(goruntime.GOARCH, memMiB*mib)
	if !ok {
		t.Skipf("test host arch %q is not supported by DamonMonitorRegion", goruntime.GOARCH)
	}

	flowOpts := &workflow.CreateContext{
		CgroupInfo: &cgroupp.Info{
			VmSnapshotSpec: cgroupp.CubeVMMResource{Memory: memMiB},
		},
	}
	s := &oci.Spec{Annotations: map[string]string{
		constants.AnnotationVMKernelCmdlineAppend: `["video=vfb:enable,720x1280M-32@60","vfb.videomemorysize=4423680","damon_reclaim.enabled=Y"]`,
	}}

	require.NoError(t, applyDamonMonitorRegionOpt(flowOpts, s))

	var params []string
	require.NoError(t, jsoniter.Unmarshal(
		[]byte(s.Annotations[constants.AnnotationVMKernelCmdlineAppend]), &params))
	assert.Contains(t, params, "video=vfb:enable,720x1280M-32@60")
	assert.Contains(t, params, "vfb.videomemorysize=4423680")
	assert.Contains(t, params, "damon_reclaim.enabled=Y")
	assert.Contains(t, params, fmt.Sprintf("damon_reclaim.monitor_region_start=%d", wantStart))
	assert.Contains(t, params, fmt.Sprintf("damon_reclaim.monitor_region_end=%d", wantEnd))
}

func TestGenDamonMonitorRegionOptSkipsAbsentAnnotation(t *testing.T) {
	flowOpts := &workflow.CreateContext{
		CgroupInfo: &cgroupp.Info{
			VmSnapshotSpec: cgroupp.CubeVMMResource{Memory: 4096},
		},
	}
	s := &oci.Spec{Annotations: map[string]string{}}

	require.NoError(t, applyDamonMonitorRegionOpt(flowOpts, s))
	assert.Empty(t, s.Annotations[constants.AnnotationVMKernelCmdlineAppend])
}

// TestGenDamonMonitorRegionOptFailsClosedWithoutCgroupInfo pins the fail-closed
// contract: with no cgroup info the guest RAM is unknown, so a DAMON request
// without explicit bounds must be rejected instead of booting with a partial
// region.
func TestGenDamonMonitorRegionOptFailsClosedWithoutCgroupInfo(t *testing.T) {
	flowOpts := &workflow.CreateContext{}
	s := &oci.Spec{Annotations: map[string]string{
		constants.AnnotationVMKernelCmdlineAppend: `["damon_reclaim.enabled=Y"]`,
	}}

	err := applyDamonMonitorRegionOpt(flowOpts, s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot derive")
}

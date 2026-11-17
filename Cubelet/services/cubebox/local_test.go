// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	imagesv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

// makeContainerConfigToSave feeds the pause package, so a liveness probe that
// is not saved here is silently dropped on resume and probe-driven restarts
// stop working.
func TestMakeContainerConfigToSaveKeepsLivenessProbe(t *testing.T) {
	cfg := &cubebox.ContainerConfig{
		Name:  "main",
		Image: &imagesv1.ImageSpec{},
		Probe: &cubebox.Probe{
			ProbeHandler: &cubebox.ProbeHandler{
				HttpGet: &cubebox.HTTPGetAction{Port: 1},
			},
		},
		LivenessProbe: &cubebox.LivenessProbe{
			ProbeHandler: &cubebox.ProbeHandler{
				HttpGet: &cubebox.HTTPGetAction{Port: 8080},
			},
			PeriodSecond:     10,
			FailureThreshold: 3,
		},
	}

	got := makeContainerConfigToSave(cfg)
	require.NotNil(t, got.GetLivenessProbe())
	assert.Equal(t, int32(8080), got.GetLivenessProbe().GetProbeHandler().GetHttpGet().GetPort())
	assert.Equal(t, int32(10), got.GetLivenessProbe().GetPeriodSecond())
	assert.Equal(t, int32(3), got.GetLivenessProbe().GetFailureThreshold())
	// The create-time readiness Probe is a different field and stays as-is.
	assert.Equal(t, int32(1), got.GetProbe().GetProbeHandler().GetHttpGet().GetPort())
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestProbeSpecsDefaultOnlyWhenEnvdPresent(t *testing.T) {
	if specs := probeSpecs(&cubeboxv1.RunCubeSandboxRequest{}); len(specs) != 0 {
		t.Fatalf("image without envd got %d probes", len(specs))
	}
	withPort := &cubeboxv1.RunCubeSandboxRequest{ExposedPorts: []int64{envdProbePort}}
	specs := probeSpecs(withPort)
	if len(specs) != 1 || specs[0].GetProbeHandler().GetHttpGet().GetPort() != int32(envdProbePort) {
		t.Fatalf("envd port probe = %+v", specs)
	}
	withAnn := &cubeboxv1.RunCubeSandboxRequest{Annotations: map[string]string{
		constants.MasterAnnotationComponentEnvdVersion: "0.1.0",
	}}
	if specs := probeSpecs(withAnn); len(specs) != 1 {
		t.Fatalf("envd annotation probe count %d", len(specs))
	}
	path := "/ready"
	user := &cubeboxv1.RunCubeSandboxRequest{
		ExposedPorts: []int64{envdProbePort},
		Containers: []*cubeboxv1.ContainerConfig{{
			LivenessProbe: &cubeboxv1.LivenessProbe{
				ProbeHandler: &cubeboxv1.ProbeHandler{
					HttpGet: &cubeboxv1.HTTPGetAction{Port: 8080, Path: &path},
				},
			},
		}},
	}
	specs = probeSpecs(user)
	if len(specs) != 1 || specs[0].GetProbeHandler().GetHttpGet().GetPort() != 8080 {
		t.Fatalf("user probe replaced: %+v", specs)
	}
}

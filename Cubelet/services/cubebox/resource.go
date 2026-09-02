// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"fmt"
	"math"
	"sync/atomic"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/numa"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func (s *service) setRequestResource(createInfo *workflow.CreateContext, reqInfo *cubebox.RunCubeSandboxRequest) error {
	var cpu int64
	var mem float64
	for _, c := range reqInfo.GetContainers() {
		cpuR, err := resource.ParseQuantity(c.GetResources().GetCpu())
		if err != nil {
			return fmt.Errorf("invalid cpu resource: %v", err)
		}
		cpu += cpuR.Value()

		memR, err := resource.ParseQuantity(c.GetResources().GetMem())
		if err != nil {
			return fmt.Errorf("invalid cpu resource: %v", err)
		}
		mem += float64(memR.Value())
	}
	createInfo.CPU = cpu
	createInfo.Memory = int64(math.Ceil(mem / 1024 / 1024 / 1024))
	createInfo.PCIMode = constants.PCIModePF
	useRoundRobinNumaNode := true
	if reqInfo.GetAnnotations() != nil {

		if numaStr, ok := reqInfo.GetAnnotations()[constants.MasterAnnotationsNumaNode]; ok {
			var numaNode int32
			if _, err := fmt.Sscanf(numaStr, "%d", &numaNode); err == nil {
				if numaNode < 0 || numaNode >= int32(numa.GetMaxNumaNodeId()) {
					return fmt.Errorf("invalid numa node: %d", numaNode)
				}

				createInfo.NumaNode = numaNode
				useRoundRobinNumaNode = false
			}
		}

		if v, ok := reqInfo.GetAnnotations()[constants.MasterAnnotationsPICMode]; ok {
			createInfo.PCIMode = v
		}
	}

	if useRoundRobinNumaNode {
		createInfo.NumaNode = s.getNextNumaNode()
	}
	return nil
}

func (s *service) getNextNumaNode() int32 {
	return int32(atomic.AddUint32(&s.numaNodeIndex, 1) % uint32(numa.GetNumaNodeCount()))
}

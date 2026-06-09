// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/affinity"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"k8s.io/apimachinery/pkg/api/resource"
)

func runInsReq2Affinity(ctx context.Context, req *types.CreateCubeSandboxReq) (context.Context, error) {
	matchExpressionsWithANDed, err := constructNodeAffinity(ctx, req)
	if err != nil {
		return ctx, err
	}

	var nodeSelectorTerms []affinity.NodeSelectorTerm
	if len(matchExpressionsWithANDed) > 0 {
		nodeSelectorTerms = append(nodeSelectorTerms, affinity.NodeSelectorTerm{MatchExpressions: matchExpressionsWithANDed})
	}

	if len(nodeSelectorTerms) > 0 {
		ns, err := affinity.NewNodeSelector(nodeSelectorTerms)
		if err != nil {
			return ctx, fmt.Errorf("runInsReq2Affinity NewNodeSelector fail: %w", err)
		}
		ctx = constants.WithNodeSelector(ctx, ns)

		if _, ok := req.Annotations[constants.AnnotationsNodeAffinityInstanceType]; !ok {
			ctx = constants.WithBackoffNodeSelector(ctx, ns)
		}
	}
	return ctx, nil
}

func constructNodeAffinity(ctx context.Context, req *types.CreateCubeSandboxReq) ([]affinity.NodeSelectorRequirement, error) {
	var matchExpressions []affinity.NodeSelectorRequirement

	nodeClusterLabel := map[string]any{}

	if scf := config.GetConfig().Scheduler; scf != nil {
		if af := scf.GetAffinityConf(req.InstanceType); af.Enable && len(af.ClusterLabels) > 0 {
			nodeClusterLabel = af.ClusterLabels
		}
	}

	if scf := config.GetConfig().Scheduler; scf != nil {
		if af := scf.GetLargeSizeAffinityConf(req.InstanceType); af.Enable {
			var tmpMatchExpressions []affinity.NodeSelectorRequirement
			if isLargeMemSize(ctx, req, af.MemoryLowerWaterMark) {
				tmpMatchExpressions = append(tmpMatchExpressions, affinity.NodeSelectorRequirement{
					Key:      constants.AffinityKeyMemorySize,
					Operator: affinity.NodeSelectorOperator(af.Operator),
					Values:   map[string]any{af.MemoryLowerWaterMark: struct{}{}},
				})
			}
			if isLargeCpucores(ctx, req, af.CpuLowerWaterMark) {
				tmpMatchExpressions = append(tmpMatchExpressions, affinity.NodeSelectorRequirement{
					Key:      constants.AffinityKeyCPUCores,
					Operator: affinity.NodeSelectorOperator(af.Operator),
					Values:   map[string]any{af.CpuLowerWaterMark: struct{}{}},
				})
			}

			if len(tmpMatchExpressions) > 0 {
				matchExpressions = append(matchExpressions, tmpMatchExpressions...)
				if len(af.ClusterLabels) > 0 {
					nodeClusterLabel = af.ClusterLabels
				}
			}
		}
	}

	if req.Annotations != nil {
		if clusterIDs, ok := req.Annotations[constants.AnnotationsNodeAffinityClusterLabel]; ok && clusterIDs != "" {
			nodeClusterLabel = utils.SliceToMap(strings.Split(clusterIDs, ":"))
		}
	}

	if len(nodeClusterLabel) > 0 {
		requiredV := affinity.NodeSelectorRequirement{
			Key:      constants.AffinityKeyClusterID,
			Operator: affinity.NodeSelectorOpIn,
			Values:   nodeClusterLabel,
		}
		matchExpressions = append(matchExpressions, requiredV)
	}

	if req.Annotations != nil {
		if instanceTypes, ok := req.Annotations[constants.AnnotationsNodeAffinityInstanceType]; ok && instanceTypes != "" {
			requiredV := affinity.NodeSelectorRequirement{
				Key:      constants.AffinityKeyInstanceType,
				Operator: affinity.NodeSelectorOpIn,
				Values:   utils.SliceToMap(strings.Split(instanceTypes, ":")),
			}
			matchExpressions = append(matchExpressions, requiredV)
		}

		if selectorJSON, ok := req.Annotations[constants.AnnotationsNodeAffinitySelector]; ok && selectorJSON != "" {
			parsed, err := parseNodeAffinitySelector(selectorJSON)
			if err != nil {
				return nil, fmt.Errorf("parseNodeAffinitySelector: %w", err)
			}
			matchExpressions = append(matchExpressions, parsed...)
		}
	}

	log.G(ctx).Debugf("constructNodeAffinity:%s", utils.InterfaceToString(matchExpressions))
	return matchExpressions, nil
}

func isLargeMemSize(ctx context.Context, req *types.CreateCubeSandboxReq, largeMemSize string) bool {
	if largeMemSize == "" {
		return false
	}
	reqMem, _ := resource.ParseQuantity("0")
	for _, ctr := range req.Containers {
		ctnmemQuantity, err := resource.ParseQuantity(ctr.Resources.Mem)
		if err != nil {
			log.G(ctx).Errorf("parse container %s mem limit: %s", ctr.Name, err.Error())
			return false
		}
		reqMem.Add(ctnmemQuantity)
	}
	return reqMem.Cmp(resource.MustParse(largeMemSize)) >= 0
}

func isLargeCpucores(ctx context.Context, req *types.CreateCubeSandboxReq, largeCpucores string) bool {
	if largeCpucores == "" {
		return false
	}
	reqCpu, _ := resource.ParseQuantity("0")
	for _, ctr := range req.Containers {
		ctncpuQuantity, err := resource.ParseQuantity(ctr.Resources.Cpu)
		if err != nil {
			log.G(ctx).Errorf("parse container %q cpu limit: %w", ctr.Name, err)
			return false
		}
		reqCpu.Add(ctncpuQuantity)
	}
	return reqCpu.Cmp(resource.MustParse(largeCpucores)) >= 0
}

// nodeSelectorRequirementJSON is an intermediate struct for JSON unmarshaling
// where Values is a slice (matching the user-facing JSON format), which is then
// converted to map[string]any to fit affinity.NodeSelectorRequirement.
type nodeSelectorRequirementJSON struct {
	Key      string                      `json:"key"`
	Operator affinity.NodeSelectorOperator `json:"operator"`
	Values   []string                    `json:"values,omitempty"`
}

func parseNodeAffinitySelector(selectorJSON string) ([]affinity.NodeSelectorRequirement, error) {
	var raw []nodeSelectorRequirementJSON
	if err := json.Unmarshal([]byte(selectorJSON), &raw); err != nil {
		return nil, err
	}

	result := make([]affinity.NodeSelectorRequirement, 0, len(raw))
	for _, r := range raw {
		if r.Key == "" {
			return nil, fmt.Errorf("node selector key must not be empty")
		}
		switch r.Operator {
		case affinity.NodeSelectorOpIn, affinity.NodeSelectorOpNotIn:
			if len(r.Values) == 0 {
				return nil, fmt.Errorf("operator %q requires non-empty values for key %q", r.Operator, r.Key)
			}
		case affinity.NodeSelectorOpExists, affinity.NodeSelectorOpDoesNotExist:
			if len(r.Values) != 0 {
				return nil, fmt.Errorf("operator %q requires empty values for key %q", r.Operator, r.Key)
			}
		switch r.Operator {
		case affinity.NodeSelectorOpIn, affinity.NodeSelectorOpNotIn:
			if len(r.Values) == 0 {
				return nil, fmt.Errorf("operator %q requires non-empty values for key %q", r.Operator, r.Key)
			}
		case affinity.NodeSelectorOpExists, affinity.NodeSelectorOpDoesNotExist:
			if len(r.Values) != 0 {
				return nil, fmt.Errorf("operator %q requires empty values for key %q", r.Operator, r.Key)
			}
		default:
			return nil, fmt.Errorf("unsupported operator %q for key %q", r.Operator, r.Key)
		}
		req := affinity.NodeSelectorRequirement{
			Key:      r.Key,
			Operator: r.Operator,
			Values:   valuesSliceToMap(r.Values),
		}
		result = append(result, req)
	}
	return result, nil
}

// valuesSliceToMap converts a []string to map[string]any.
// This bridges the gap between the JSON input format (values as array) and the
// NodeSelectorRequirement.Values type (map[string]any).
func valuesSliceToMap(vals []string) map[string]any {
	if len(vals) == 0 {
		return nil
	}
	m := make(map[string]any, len(vals))
	for _, v := range vals {
		m[v] = struct{}{}
	}
	return m
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"errors"
	"fmt"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"sort"
	"strings"
)

func resolveTemplateNodes(instanceType string, scope []string) ([]*node.Node, error) {
	nodes := healthyTemplateNodes(instanceType)
	if len(nodes) == 0 {
		return nil, ErrNoTemplateNodes
	}
	if len(scope) == 0 {
		return nodes, nil
	}
	allowed := make(map[string]struct{}, len(scope))
	for _, item := range scope {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		allowed[item] = struct{}{}
	}
	selected := make([]*node.Node, 0, len(nodes))
	matched := make(map[string]struct{})
	for _, item := range nodes {
		if item == nil {
			continue
		}
		if _, ok := allowed[item.ID()]; ok {
			selected = append(selected, item)
			matched[item.ID()] = struct{}{}
			continue
		}
		if _, ok := allowed[item.HostIP()]; ok {
			selected = append(selected, item)
			matched[item.HostIP()] = struct{}{}
		}
	}
	missing := make([]string, 0)
	for _, item := range scope {
		if _, ok := matched[item]; ok {
			continue
		}
		missing = append(missing, item)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("target nodes are not healthy or not found: %s", strings.Join(missing, ","))
	}
	if len(selected) == 0 {
		return nil, ErrNoTemplateNodes
	}
	return selected, nil
}

func normalizeTemplateImageRequest(req *types.CreateTemplateFromImageReq) (*types.CreateTemplateFromImageReq, error) {
	if req == nil {
		return nil, errors.New("request is nil")
	}
	if req.Request == nil || strings.TrimSpace(req.RequestID) == "" {
		return nil, errors.New("requestID is required")
	}
	if strings.TrimSpace(req.SourceImageRef) == "" {
		return nil, errors.New("source_image_ref is required")
	}
	if strings.HasPrefix(strings.TrimSpace(req.SourceImageRef), "-") {
		return nil, errors.New("source_image_ref must not start with '-'")
	}
	if strings.TrimSpace(req.WritableLayerSize) == "" {
		return nil, errors.New("writable_layer_size is required")
	}
	cloned := *req
	exposedPorts, err := normalizeTemplateExposedPorts(req.ExposedPorts)
	if err != nil {
		return nil, err
	}
	cloned.ExposedPorts = exposedPorts
	// Always auto-generate the template ID. Users are not allowed to set
	// custom template IDs because the snapshot system depends on the
	// tpl- / snap- prefix convention for storage naming and identification.
	cloned.TemplateID = generateTemplateID()
	if cloned.InstanceType == "" {
		cloned.InstanceType = cubeboxv1.InstanceType_cubebox.String()
	}
	if cloned.NetworkType == "" {
		cloned.NetworkType = cubeboxv1.NetworkType_tap.String()
	}
	return &cloned, nil
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func normalizeTemplateExposedPorts(ports []int32) ([]int32, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	uniq := make(map[int32]struct{}, len(ports))
	normalized := make([]int32, 0, len(ports))
	for _, port := range ports {
		if port <= 0 || port > 65535 {
			return nil, fmt.Errorf("invalid exposed port %d", port)
		}
		if _, exists := uniq[port]; exists {
			continue
		}
		uniq[port] = struct{}{}
		normalized = append(normalized, port)
	}
	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i] < normalized[j]
	})
	if countCustomTemplateExposedPorts(normalized) > 3 {
		return nil, fmt.Errorf("at most 3 custom exposed ports are supported")
	}
	return normalized, nil
}

func countCustomTemplateExposedPorts(ports []int32) int {
	reserved := defaultTemplateExposedPorts()
	count := 0
	for _, port := range ports {
		if _, ok := reserved[port]; ok {
			continue
		}
		count++
	}
	return count
}

func defaultTemplateExposedPorts() map[int32]struct{} {
	return map[int32]struct{}{
		49983: {},
	}
}

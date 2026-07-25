// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"

	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/image"
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
	sourceImageRef := strings.TrimSpace(req.SourceImageRef)
	if sourceImageRef == "" {
		return nil, errors.New("source_image_ref is required")
	}
	if err := image.ValidateImageRef(sourceImageRef); err != nil {
		return nil, fmt.Errorf("source_image_ref: %w", err)
	}
	if strings.TrimSpace(req.WritableLayerSize) == "" {
		return nil, errors.New("writable_layer_size is required")
	}
	cloned := *req
	cloned.SourceImageRef = sourceImageRef
	exposedPorts, err := normalizeTemplateExposedPorts(req.ExposedPorts)
	if err != nil {
		return nil, err
	}
	cloned.ExposedPorts = exposedPorts
	if err := validateTemplateAlias(cloned.Alias); err != nil {
		return nil, err
	}
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
	if cloned.EnableIvshmem != nil && !*cloned.EnableIvshmem {
		cloned.EnableIvshmem = nil
	}
	if err := validateTemplateCubeNetworkConfig(cloned.CubeNetworkConfig); err != nil {
		return nil, err
	}
	return &cloned, nil
}

// aliasValidationRe matches the allowed alias charset: lowercase alphanumeric
// and hyphens, starting with an alphanumeric character, max 64 characters.
var aliasValidationRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// validateTemplateAlias validates that an alias conforms to the required
// format: lowercase alphanumeric and hyphens only, must start with an
// alphanumeric character, max 64 characters, and must NOT collide with the
// template ID prefix convention (tpl-/snap-). An empty alias is valid (no
// alias requested).
func validateTemplateAlias(alias string) error {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return nil
	}
	if hasValidTemplateIDPrefix(alias) {
		return fmt.Errorf("alias %q must not start with 'tpl-' or 'snap-'", alias)
	}
	if !aliasValidationRe.MatchString(alias) {
		return fmt.Errorf("alias %q is invalid: must match ^[a-z0-9][a-z0-9-]{0,63}$ (lowercase alphanumeric and hyphens, max 64 chars)", alias)
	}
	return nil
}

func validateTemplateCubeNetworkConfig(cfg *types.CubeNetworkConfig) error {
	if cfg == nil || !hasDomainAllowOutTarget(cfg.AllowOut) {
		return nil
	}
	if cfg.AllowInternetAccess != nil && !*cfg.AllowInternetAccess {
		return nil
	}
	if hasDenyAllIPv4Target(cfg.DenyOut) {
		return nil
	}
	return errors.New("when specifying allowed domains in allow_out, you must disable public outbound traffic or include '0.0.0.0/0' in deny_out to block all other traffic")
}

func hasDomainAllowOutTarget(targets []string) bool {
	for _, target := range targets {
		if isDomainAllowOutTarget(target) {
			return true
		}
	}
	return false
}

func hasDenyAllIPv4Target(targets []string) bool {
	for _, target := range targets {
		if strings.TrimSpace(target) == "0.0.0.0/0" {
			return true
		}
	}
	return false
}

func isDomainAllowOutTarget(target string) bool {
	target = strings.TrimSpace(target)
	if target == "" || strings.Contains(target, "/") || net.ParseIP(target) != nil {
		return false
	}
	if isDottedDecimalLikeTarget(target) {
		return false
	}
	domain := strings.ToLower(strings.TrimSuffix(target, "."))
	if strings.HasPrefix(domain, "*.") {
		domain = strings.TrimPrefix(domain, "*.")
	} else if strings.Contains(domain, "*") {
		return false
	}
	return isValidDNSDomainName(domain)
}

func isDottedDecimalLikeTarget(target string) bool {
	parts := strings.Split(strings.TrimSuffix(target, "."), ".")
	if len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return false
			}
		}
	}
	return true
}

func isValidDNSDomainName(domain string) bool {
	if domain == "" || len(domain) >= 255 {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, ch := range label {
			if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
				continue
			}
			return false
		}
	}
	return true
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

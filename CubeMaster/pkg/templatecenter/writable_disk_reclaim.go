// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// writableDiskReclaimFromCreate reads the template-owned reclaim policy from a
// create request. Empty annotations (including historical templates that
// predate the field) mean disabled.
func writableDiskReclaimFromCreate(req *sandboxtypes.CreateCubeSandboxReq) (enabled bool, present bool, err error) {
	if req == nil || req.Annotations == nil {
		return false, false, nil
	}
	raw, ok := req.Annotations[constants.CubeAnnotationWritableDiskReclaim]
	if !ok {
		return false, false, nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true":
		return true, true, nil
	case "false":
		return false, true, nil
	default:
		return false, true, fmt.Errorf("invalid %s value %q: supported values are \"true\" and \"false\"",
			constants.CubeAnnotationWritableDiskReclaim, raw)
	}
}

// ApplyWritableDiskReclaimFromTemplate makes the value persisted at template
// creation authoritative over a caller annotation. A matching caller value is
// accepted; a conflict is rejected before either resolution path merges the
// template map and erases the evidence. The stored template map is never
// mutated. Disabled policies are normalized to an absent annotation so
// historical request shapes stay unchanged.
func ApplyWritableDiskReclaimFromTemplate(req, templateReq *sandboxtypes.CreateCubeSandboxReq) error {
	if req == nil {
		return nil
	}
	templateEnabled, _, err := writableDiskReclaimFromCreate(templateReq)
	if err != nil {
		return fmt.Errorf("stored template writable disk reclaim policy: %w", err)
	}
	callerEnabled, callerPresent, err := writableDiskReclaimFromCreate(req)
	if err != nil {
		return err
	}
	if callerPresent && callerEnabled != templateEnabled {
		return fmt.Errorf("%s is template-owned: the template sets it to %t but the request sets it to %t",
			constants.CubeAnnotationWritableDiskReclaim, templateEnabled, callerEnabled)
	}
	if templateEnabled {
		if req.Annotations == nil {
			req.Annotations = make(map[string]string)
		}
		req.Annotations[constants.CubeAnnotationWritableDiskReclaim] = "true"
		return nil
	}
	delete(req.Annotations, constants.CubeAnnotationWritableDiskReclaim)
	return nil
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"encoding/json"
	"fmt"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func envdInitEnvVars(req *types.CreateCubeSandboxReq) (map[string]string, error) {
	// Non-envd images must not gain an /init dependency merely because they
	// use runtime CA defaults. Preinstalled envd is detected during snapshot
	// collection; injected envd is known before the template's first start.
	hasEnvd := req.Annotations[constants.CubeAnnotationsInjectEnvd] == constants.CubeAnnotationsInjectEnvdOptIn ||
		req.Annotations[constants.CubeAnnotationComponentEnvdVersion] != ""
	raw := req.Annotations[constants.CubeAnnotationRuntimeCAEnv]
	if !hasEnvd || raw == "" {
		return req.CreateTimeEnvVars, nil
	}
	envs := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &envs); err != nil {
		return nil, fmt.Errorf("invalid runtime CA environment: %w", err)
	}
	if envs == nil {
		envs = map[string]string{}
	}
	for key, value := range req.CreateTimeEnvVars {
		// Presence, including an explicit empty value, overrides the default.
		envs[key] = value
	}
	return envs, nil
}

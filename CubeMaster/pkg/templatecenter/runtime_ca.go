// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"fmt"
	"regexp"
	"slices"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

var runtimeCAEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func normalizeRuntimeCAEnvNames(names []string) ([]string, error) {
	if names == nil {
		return nil, nil
	}
	for _, name := range names {
		if !runtimeCAEnvNamePattern.MatchString(name) {
			return nil, fmt.Errorf("cube_ca_env_vars: invalid environment variable name %q", name)
		}
	}
	return canonicalRuntimeCAEnvNames(names), nil
}

func canonicalRuntimeCAEnvNames(names []string) []string {
	out := make([]string, len(names))
	copy(out, names)
	slices.Sort(out)
	return slices.Compact(out)
}

func runtimeCAEnvNames(req *types.CreateTemplateFromImageReq) []string {
	names := req.CubeCAEnvVars
	if names == nil {
		names = []string{"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "CARGO_HTTP_CAINFO"}
	}
	// Normalized at submission; canonicalize here too for fingerprint callers.
	return canonicalRuntimeCAEnvNames(names)
}

func runtimeCAEnvEnabled(req *types.CreateTemplateFromImageReq) bool {
	return req != nil && (req.WithCubeCA == nil || *req.WithCubeCA) &&
		(req.WithCubeCAEnv == nil || *req.WithCubeCAEnv) && len(runtimeCAEnvNames(req)) > 0
}

func runtimeCATrustVersion(req *types.CreateTemplateFromImageReq, fingerprint string) string {
	if fingerprint != "" && runtimeCAEnvEnabled(req) {
		// Older artifacts do not record a verified bundle path and may have
		// only an anchor file. Build a new artifact instead of guessing a path.
		return "1"
	}
	return ""
}

func runtimeCAEnvDefaults(req *types.CreateTemplateFromImageReq, bundle string) []*types.KeyValue {
	var envs []*types.KeyValue
	for _, name := range runtimeCAEnvNames(req) {
		envs = append(envs, &types.KeyValue{Key: name, Value: bundle})
	}
	return envs
}

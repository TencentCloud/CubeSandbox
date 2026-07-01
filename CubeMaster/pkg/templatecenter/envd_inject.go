// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

const (
	envdHostDirDefault        = "/usr/local/share/cubesandbox-envd"
	envdHostDirEnv            = "CUBE_MASTER_ENVD_HOST_DIR"
	envdBinaryName            = "envd"
	envdInjectAnnotationOptIn = "true"
	envdInjectionFileMode     = 0o755
	envdInjectionDirMode      = 0o755
)

func envdHostPath(req *types.CreateTemplateFromImageReq) string {
	if req != nil && req.EnvdPath != "" {
		return req.EnvdPath
	}
	dir := os.Getenv(envdHostDirEnv)
	if dir == "" {
		dir = envdHostDirDefault
	}
	return filepath.Join(dir, envdBinaryName)
}

func shouldInjectEnvdIntoTemplate(req *types.CreateTemplateFromImageReq) bool {
	if req == nil || req.ContainerOverrides == nil || req.ContainerOverrides.Annotations == nil {
		return false
	}
	return req.ContainerOverrides.Annotations[constants.CubeAnnotationsInjectEnvd] == envdInjectAnnotationOptIn
}

type envdInjectionPayload struct {
	HostPath string
	SHA256   string
	Data     []byte
}

func prepareEnvdInjectionPayload(req *types.CreateTemplateFromImageReq) (*envdInjectionPayload, error) {
	if !shouldInjectEnvdIntoTemplate(req) {
		return nil, nil
	}
	srcPath := envdHostPath(req)
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, fmt.Errorf("envd-inject: read %q (set %s or --envd-path to override): %w", srcPath, envdHostDirEnv, err)
	}
	sum := sha256.Sum256(data)
	return &envdInjectionPayload{
		HostPath: srcPath,
		SHA256:   hex.EncodeToString(sum[:]),
		Data:     data,
	}, nil
}

func injectEnvdIntoRootfs(ctx context.Context, rootfsDir string, req *types.CreateTemplateFromImageReq) (string, error) {
	payload, err := prepareEnvdInjectionPayload(req)
	if err != nil {
		return "", err
	}
	return injectEnvdPayloadIntoRootfs(ctx, rootfsDir, payload)
}

func injectEnvdPayloadIntoRootfs(ctx context.Context, rootfsDir string, payload *envdInjectionPayload) (string, error) {
	if payload == nil {
		return "", nil
	}
	dstDir := filepath.Join(rootfsDir, filepath.Dir(constants.CubeEnvdInImagePath))
	if err := os.MkdirAll(dstDir, envdInjectionDirMode); err != nil {
		return "", fmt.Errorf("envd-inject: mkdir %q: %w", dstDir, err)
	}
	dstPath := filepath.Join(rootfsDir, constants.CubeEnvdInImagePath)
	if err := os.WriteFile(dstPath, payload.Data, envdInjectionFileMode); err != nil {
		_ = os.Remove(dstPath)
		return "", fmt.Errorf("envd-inject: write %q: %w", dstPath, err)
	}
	log.G(ctx).Infof("envd-inject: copied %s -> rootfs%s sha256=%s", payload.HostPath, constants.CubeEnvdInImagePath, payload.SHA256)
	return payload.SHA256, nil
}

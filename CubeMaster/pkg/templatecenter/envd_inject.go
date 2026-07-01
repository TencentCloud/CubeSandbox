// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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
	envdInImageDir            = "/usr/local/bin"
	envdInImagePath           = "/usr/local/bin/envd"
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

func injectEnvdIntoRootfs(ctx context.Context, rootfsDir string, req *types.CreateTemplateFromImageReq) (string, error) {
	if !shouldInjectEnvdIntoTemplate(req) {
		return "", nil
	}
	srcPath := envdHostPath(req)
	src, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("envd-inject: open %q (set %s to override): %w", srcPath, envdHostDirEnv, err)
	}
	defer src.Close()

	dstDir := filepath.Join(rootfsDir, envdInImageDir)
	if err := os.MkdirAll(dstDir, envdInjectionDirMode); err != nil {
		return "", fmt.Errorf("envd-inject: mkdir %q: %w", dstDir, err)
	}
	dstPath := filepath.Join(rootfsDir, envdInImagePath)
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, envdInjectionFileMode)
	if err != nil {
		return "", fmt.Errorf("envd-inject: create %q: %w", dstPath, err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(dst, io.TeeReader(src, hasher)); err != nil {
		_ = dst.Close()
		_ = os.Remove(dstPath)
		return "", fmt.Errorf("envd-inject: copy to %q: %w", dstPath, err)
	}
	if err := dst.Close(); err != nil {
		return "", fmt.Errorf("envd-inject: close %q: %w", dstPath, err)
	}
	sha := hex.EncodeToString(hasher.Sum(nil))
	log.G(ctx).Infof("envd-inject: copied %s -> rootfs%s sha256=%s", srcPath, envdInImagePath, sha)
	return sha, nil
}

func envdBinarySHA256(req *types.CreateTemplateFromImageReq) (string, error) {
	src, err := os.Open(envdHostPath(req))
	if err != nil {
		return "", err
	}
	defer src.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, src); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

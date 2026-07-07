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
	envdHostDirDefault    = "/usr/local/share/cubesandbox-envd"
	envdHostDirEnv        = "CUBE_MASTER_ENVD_HOST_DIR"
	envdBinaryName        = "envd"
	envdInjectionFileMode = 0o755
	envdInjectionDirMode  = 0o755
)

func envdHostPath(req *types.CreateTemplateFromImageReq) string {
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
	return req.ContainerOverrides.Annotations[constants.CubeAnnotationsInjectEnvd] == constants.CubeAnnotationsInjectEnvdOptIn
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
	if err := validateHostEnvdPath(srcPath); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, fmt.Errorf("envd-inject: read %q (set %s to override): %w", srcPath, envdHostDirEnv, err)
	}
	if err := validateEnvdELF(srcPath, data); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return &envdInjectionPayload{
		HostPath: srcPath,
		SHA256:   hex.EncodeToString(sum[:]),
		Data:     data,
	}, nil
}

func validateHostEnvdPath(srcPath string) error {
	info, err := os.Lstat(srcPath)
	if err != nil {
		return fmt.Errorf("envd-inject: stat %q (set %s to override): %w", srcPath, envdHostDirEnv, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("envd-inject: %q must not be a symlink", srcPath)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("envd-inject: %q must be a regular file", srcPath)
	}
	if info.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("envd-inject: %q must not be world-writable", srcPath)
	}
	dir := filepath.Dir(srcPath)
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("envd-inject: stat dir %q: %w", dir, err)
	}
	if dirInfo.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("envd-inject: dir %q must not be world-writable", dir)
	}
	return nil
}

func validateEnvdELF(srcPath string, data []byte) error {
	if len(data) < 4 || data[0] != 0x7f || data[1] != 'E' || data[2] != 'L' || data[3] != 'F' {
		return fmt.Errorf("envd-inject: %q must be an ELF binary", srcPath)
	}
	return nil
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

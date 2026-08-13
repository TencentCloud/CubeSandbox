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
	envdInjectionFileMode = 0o755
	envdInjectionDirMode  = 0o755
)

func ShouldInjectEnvdIntoTemplate(req *types.CreateTemplateFromImageReq) bool {
	if req == nil || req.ContainerOverrides == nil || req.ContainerOverrides.Annotations == nil {
		return false
	}
	return req.ContainerOverrides.Annotations[constants.CubeAnnotationsInjectEnvd] == constants.CubeAnnotationsInjectEnvdOptIn
}

type EnvdInjectionPayload struct {
	SHA256 string
	Data   []byte
}

func (p *EnvdInjectionPayload) ReleaseData() {
	if p != nil {
		p.Data = nil
	}
}

func NewEnvdInjectionPayloadFromBytes(data []byte) (*EnvdInjectionPayload, error) {
	if err := validateEnvdPayloadBytes(data); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return &EnvdInjectionPayload{
		SHA256: hex.EncodeToString(sum[:]),
		Data:   data,
	}, nil
}

func validateEnvdPayloadBytes(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("envd-inject: uploaded envd must not be empty")
	}
	if len(data) > constants.MaxEnvdPayloadBytes {
		return fmt.Errorf("envd-inject: uploaded envd exceeds 16MiB")
	}
	if len(data) < 4 || data[0] != 0x7f || data[1] != 'E' || data[2] != 'L' || data[3] != 'F' {
		return fmt.Errorf("envd-inject: uploaded envd must be an ELF binary")
	}
	return nil
}

func injectEnvdPayloadIntoRootfs(ctx context.Context, rootfsDir string, payload *EnvdInjectionPayload) (string, error) {
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
	log.G(ctx).Infof("envd-inject: wrote uploaded envd -> rootfs%s sha256=%s", constants.CubeEnvdInImagePath, payload.SHA256)
	return payload.SHA256, nil
}

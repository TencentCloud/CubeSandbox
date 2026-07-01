// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

const (
	envdInImagePathDefault = "/usr/local/bin/envd"
	envdInImagePathEnv     = "CUBE_MASTER_ENVD_IN_IMAGE_PATH"
	envdDefaultPort        = 49983
)

func envdInImagePath() string {
	if v := os.Getenv(envdInImagePathEnv); v != "" {
		return v
	}
	return envdInImagePathDefault
}

func injectEnvdSidecar(ctx context.Context, req *types.CreateCubeSandboxReq, out *cubebox.RunCubeSandboxRequest) error {
	val, ok := req.Annotations[constants.CubeAnnotationsInjectEnvd]
	if !ok || val != "true" {
		return nil
	}
	if req.InstanceType != "" && req.InstanceType != cubebox.InstanceType_cubebox.String() {
		log.G(ctx).Infof("envd: skipping injection for instance_type=%q (only cubebox is supported)", req.InstanceType)
		return nil
	}
	if len(req.Containers) == 0 {
		return nil
	}
	mainContainer := req.Containers[0]
	if isEnvdEntrypointWrapped(mainContainer) {
		log.G(ctx).Infof("envd: container %q entrypoint already wrapped, skipping", mainContainer.Name)
		return nil
	}
	wrapEntrypointForEnvd(mainContainer)
	out.ExposedPorts = append(out.ExposedPorts, envdDefaultPort)
	log.G(ctx).Infof("envd: wrapped entrypoint of container %q (envd path=%s, baked into template rootfs)", mainContainer.Name, envdInImagePath())
	return nil
}

func wrapEntrypointForEnvd(c *types.Container) {
	originalCmd := buildOriginalCommand(c)
	c.Command = []string{"/bin/sh", "-c"}
	c.Args = []string{
		fmt.Sprintf(`%s >/var/log/envd.log 2>&1 & exec "$@"`, envdInImagePath()),
		"--",
	}
	c.Args = append(c.Args, originalCmd...)
}

func isEnvdEntrypointWrapped(c *types.Container) bool {
	if c == nil || len(c.Command) < 2 || len(c.Args) < 1 {
		return false
	}
	if c.Command[0] != "/bin/sh" || c.Command[1] != "-c" {
		return false
	}
	return strings.Contains(c.Args[0], envdInImagePath())
}

func buildOriginalCommand(c *types.Container) []string {
	var cmd []string
	cmd = append(cmd, c.Command...)
	cmd = append(cmd, c.Args...)
	if len(cmd) == 0 {
		return []string{"sleep", "infinity"}
	}
	return cmd
}

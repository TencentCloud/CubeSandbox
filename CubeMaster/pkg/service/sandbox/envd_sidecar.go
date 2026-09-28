// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

const envdDefaultPort = 49983

func injectEnvdSidecar(ctx context.Context, req *types.CreateCubeSandboxReq, out *cubebox.RunCubeSandboxRequest) {
	val, ok := req.Annotations[constants.CubeAnnotationsInjectEnvd]
	if !ok || val != constants.CubeAnnotationsInjectEnvdOptIn {
		return
	}
	if req.InstanceType != "" && req.InstanceType != cubebox.InstanceType_cubebox.String() {
		log.G(ctx).Infof("envd: skipping injection for instance_type=%q (only cubebox is supported)", req.InstanceType)
		return
	}
	if len(req.Containers) == 0 {
		return
	}
	mainContainer := req.Containers[0]
	if isEnvdEntrypointWrapped(mainContainer) {
		log.G(ctx).Infof("envd: container %q entrypoint already wrapped, skipping", mainContainer.Name)
		return
	}
	wrapEntrypointForEnvd(mainContainer)
	appendExposedPortIfMissing(out, envdDefaultPort)
	log.G(ctx).Infof("envd: wrapped entrypoint of container %q (envd path=%s, pre-baked at build time)", mainContainer.Name, constants.CubeEnvdInImagePath)
}

func wrapEntrypointForEnvd(c *types.Container) {
	originalCmd := buildOriginalCommand(c)
	c.Command = []string{"/bin/sh", "-c"}
	c.Args = []string{
		fmt.Sprintf(`%s >/var/log/envd.log 2>&1 & exec "$@"`, constants.CubeEnvdInImagePath),
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
	return strings.Contains(c.Args[0], constants.CubeEnvdInImagePath)
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

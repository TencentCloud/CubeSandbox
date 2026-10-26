// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubelet

import (
	"github.com/containerd/containerd/v2/core/sandbox"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
)

func GetInMemorySandboxControllers(ic *plugin.InitContext) (map[string]sandbox.Controller, error) {
	sc := make(map[string]sandbox.Controller)
	sandboxers, err := ic.GetByType(plugins.SandboxControllerPlugin)
	if err != nil {
		return nil, err
	}
	for name, p := range sandboxers {
		sc[name] = p.(sandbox.Controller)
	}
	return sc, nil
}

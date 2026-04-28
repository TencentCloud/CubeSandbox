// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package shimapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/containerd/containerd/v2/client"
	"encoding/json"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/virtiofs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/cubelog"
	"k8s.io/apimachinery/pkg/util/sets"
)

type CubeVirtioFSAPI interface {
	AddAllowedDirs(ctx context.Context, dirs []string) error
}

func (csc *cubeShimControl) AddAllowedDirs(ctx context.Context, toAppendLayer []string) error {
	cubebox := csc.cubebox

	if len(toAppendLayer) > 0 {
		logEntry := log.G(ctx).WithFields(CubeLog.Fields{
			"cubebox": cubebox.ID,
			"task":    csc.task.ID(),
			"action":  "AddAllowedDirs",
		})
		if cubebox.VirtiofsMap == nil {
			cfg, _ := virtiofs.GenVirtiofsConfig([]string{})
			cubebox.VirtiofsMap = map[string]*virtiofs.VirtiofsConfig{
				constants.CubeDefaultNamespace: cfg,
			}
		}

		defaultVfs := cubebox.VirtiofsMap[constants.CubeDefaultNamespace]
		allowedDir := sets.New(defaultVfs.VirtioBackendFsConfig.AllowedDirs...)
		for _, hostLay := range toAppendLayer {
			allowedDir.Insert(hostLay)
		}

		defaultVfs.VirtioBackendFsConfig.AllowedDirs = allowedDir.UnsortedList()
		cubebox.VirtiofsMap[constants.CubeDefaultNamespace] = defaultVfs
		cubeFsValue, err := json.Marshal(defaultVfs)
		if err != nil {
			logEntry.WithError(err).Errorf("failed to marshal cube fs config")
			return fmt.Errorf("failed to marshal cube fs config")
		}
		if err := csc.task.Update(ctx, client.WithAnnotations(map[string]string{
			constants.AnnotationsFSKey: cubeFsValue,
		})); err != nil {
			logEntry.WithError(err).Errorf("failed to update task for container %s", cubebox.FirstContainer().Container.ID())
			return fmt.Errorf("failed to update task for container %s", cubebox.FirstContainer().Container.ID())
		}
		logEntry.WithField("cube.fs", string(cubeFsValue)).Infof("")
	}
	return nil
}

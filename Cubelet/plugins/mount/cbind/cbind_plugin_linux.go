// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package cbind

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/containerd/platforms"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
)

var forceloop bool

type cubeBindMountHandler struct {
}

func newCubeBindMountHandler() mount.Handler {
	return cubeBindMountHandler{}
}

func (cubeBindMountHandler) Mount(ctx context.Context, m mount.Mount, mp string, _ []mount.ActiveMount) (mount.ActiveMount, error) {
	if m.Type != "cmount" {
		return mount.ActiveMount{}, errdefs.ErrNotImplemented
	}

	var mountpoint string
	for _, v := range m.Options {
		if strings.HasPrefix(v, "mountpoint=") {
			mountpoint = strings.TrimPrefix(v, "mountpoint=")
			break
		}
	}

	newOptions := make([]string, 0)
	for _, v := range m.Options {
		if !strings.HasPrefix(v, "mountpoint=") {
			newOptions = append(newOptions, v)
		}
	}
	m.Options = newOptions

	if mountpoint == "" {
		mountpoint = mp
	}

	if err := os.MkdirAll(mountpoint, 0700); err != nil {
		return mount.ActiveMount{}, err
	}

	err := m.Mount(mountpoint)
	if err != nil {
		return mount.ActiveMount{}, fmt.Errorf("cmount %s to %s failed: %w", m.Source, mountpoint, err)
	}

	t := time.Now()
	return mount.ActiveMount{
		Mount:      m,
		MountedAt:  &t,
		MountPoint: mountpoint,
	}, nil
}

func (cubeBindMountHandler) Unmount(ctx context.Context, path string) error {
	return mount.Unmount(path, 0)
}

type Config struct{}

func init() {
	registry.Register(&plugin.Registration{
		Type:   plugins.MountHandlerPlugin,
		ID:     "cmount",
		Config: &Config{},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			p := platforms.DefaultSpec()
			p.OS = runtime.GOOS
			ic.Meta.Platforms = append(ic.Meta.Platforms, p)

			return newCubeBindMountHandler(), nil
		},
	})
}

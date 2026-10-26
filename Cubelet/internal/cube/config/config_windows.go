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

package config

import (
	"os"
	"path/filepath"

	"github.com/containerd/containerd/v2/defaults"
)

func DefaultImageConfig() ImageConfig {
	return ImageConfig{
		Snapshotter:            defaults.DefaultSnapshotter,
		StatsCollectPeriod:     10,
		MaxConcurrentDownloads: 3,
		ImageDecryption: ImageDecryption{
			KeyModel: KeyModelNode,
		},
		PinnedImages: map[string]string{
			"sandbox": DefaultSandboxImage,
		},
		ImagePullProgressTimeout: defaultImagePullProgressTimeoutDuration.String(),
	}
}

func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		CniConfig: CniConfig{
			NetworkPluginBinDir:        filepath.Join(os.Getenv("ProgramFiles"), "containerd", "cni", "bin"),
			NetworkPluginConfDir:       filepath.Join(os.Getenv("ProgramFiles"), "containerd", "cni", "conf"),
			NetworkPluginMaxConfNum:    1,
			NetworkPluginSetupSerially: false,
			NetworkPluginConfTemplate:  "",
			UseInternalLoopback:        false,
		},
		ContainerdConfig: ContainerdConfig{
			DefaultRuntimeName: "runhcs-wcow-process",
			Runtimes: map[string]Runtime{
				"runhcs-wcow-process": {
					Type:                 "io.containerd.runhcs.v1",
					ContainerAnnotations: []string{"io.microsoft.container.*"},
				},
				"runhcs-wcow-hypervisor": {
					Type:                 "io.containerd.runhcs.v1",
					PodAnnotations:       []string{"io.microsoft.virtualmachine.*"},
					ContainerAnnotations: []string{"io.microsoft.container.*"},

					Options: map[string]interface{}{

						"SandboxIsolation": 1,

						"ScaleCpuLimitsToSandbox": true,
					},
				},
			},
		},
		MaxContainerLogLineSize:   16 * 1024,
		IgnoreImageDefinedVolumes: false,

		DrainExecSyncIOTimeout: "0s",
	}
}

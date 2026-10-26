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

package annotations

import (
	"github.com/containerd/containerd/v2/pkg/oci"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	customopts "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/opts"
)

const (
	ContainerTypeSandbox = "sandbox"

	ContainerTypeContainer = "container"

	ContainerType = "io.kubernetes.cri.container-type"

	SandboxID = "io.kubernetes.cri.sandbox-id"

	SandboxCPUPeriod = "io.kubernetes.cri.sandbox-cpu-period"
	SandboxCPUQuota  = "io.kubernetes.cri.sandbox-cpu-quota"
	SandboxCPUShares = "io.kubernetes.cri.sandbox-cpu-shares"

	SandboxMem = "io.kubernetes.cri.sandbox-memory"

	SandboxLogDir = "io.kubernetes.cri.sandbox-log-directory"

	UntrustedWorkload = "io.kubernetes.cri.untrusted-workload"

	SandboxNamespace = "io.kubernetes.cri.sandbox-namespace"

	SandboxUID = "io.kubernetes.cri.sandbox-uid"

	SandboxName = "io.kubernetes.cri.sandbox-name"

	ContainerName = "io.kubernetes.cri.container-name"

	ImageName = "io.kubernetes.cri.image-name"

	SandboxImageName = "io.kubernetes.cri.podsandbox.image-name"

	PodAnnotations = "io.kubernetes.cri.pod-annotations"

	RuntimeHandler = "io.containerd.cri.runtime-handler"

	WindowsHostProcess = "microsoft.com/hostprocess-container"
)

func DefaultCRIAnnotations(
	sandboxID string,
	containerName string,
	imageName string,
	config *runtime.PodSandboxConfig,
	sandbox bool,
) []oci.SpecOpts {
	opts := []oci.SpecOpts{
		customopts.WithAnnotation(SandboxID, sandboxID),
		customopts.WithAnnotation(SandboxNamespace, config.GetMetadata().GetNamespace()),
		customopts.WithAnnotation(SandboxUID, config.GetMetadata().GetUid()),
		customopts.WithAnnotation(SandboxName, config.GetMetadata().GetName()),
	}
	ctrType := ContainerTypeContainer
	if sandbox {
		ctrType = ContainerTypeSandbox

		opts = append(
			opts,
			customopts.WithAnnotation(SandboxLogDir, config.GetLogDirectory()),
			customopts.WithAnnotation(SandboxImageName, imageName),
		)
	} else {

		opts = append(
			opts,
			customopts.WithAnnotation(ContainerName, containerName),
			customopts.WithAnnotation(ImageName, imageName),
		)
	}
	return append(opts, customopts.WithAnnotation(ContainerType, ctrType))
}

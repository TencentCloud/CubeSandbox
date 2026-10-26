// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package constants

const (
	LabelImagePrefix = "io.containerd.image/"

	LabelImageConvertedCubeSchemaLabelKey = LabelImagePrefix + "converted-cube-schema"

	LabelImageUidFiles = LabelImagePrefix + "uid-files"

	LabelImageHostLowerDirs       = LabelImagePrefix + "host-lower-dirs"
	LabelImageHostLowerDirsPrefix = LabelImagePrefix + "host-lower-dirs/prefix"

	LabelImageNoHostLayers = LabelImagePrefix + "no-host-layers"

	LabelImageLayerDirs      = LabelImagePrefix + "layer/dirs"
	LabelImageCreateBy       = LabelImagePrefix + "create-by"
	LabelImageCreateByImport = "import"
	LabelCubeNamespace       = "namespace"
)

const (
	LabelCriSandboxID     = "io.kubernetes.cri.sandbox-id"
	LabelCriContainerType = "io.cri-containerd.kind"

	LabelSandboxFirstContainerID = "cube.sandbox.first.container.id"
)

const (
	ContainerTypeContainer = "container"

	ContainerTypeSandBox = "sandbox"

	ContainerType = "io.kubernetes.cri.container-type"

	SandboxID = "io.kubernetes.cri.sandbox-id"

	ContainerName = "io.kubernetes.cri.container-name"
)

const (
	LabelInstanceType = "cube.cloud.tencentcloud.com/instance-type"
)

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package constants

const (
	AnnotationCFSRootfs         = "cube.image.cfs.rootfs"
	LabelContainerImageMedia    = "cube.image.media"
	LabelContainerImageRootInfo = "cube.image.rootinfo"
	LabelContainerCubeImageSpec = "cube.image.cube.image.spec"

	AnnotationCubeletNameSpace = "cube.cubelet/namespace"
)

const (
	AnnotationContainerdPrefix    = "io.containerd"
	AnnotationContainerdRefSource = AnnotationContainerdPrefix + ".import.ref-source"
	AnnotationValueAnnotation     = "annotation"
)

const (
	AnnotationSnapshotterPrefix                    = "containerd.io/snapshot/"
	AnnotationSnapshotterExternalPath              = AnnotationSnapshotterPrefix + "external-path"
	AnnotationSnapshotterUseExternalBind           = AnnotationSnapshotterPrefix + "use-external-bind"
	AnnotationSnapshotUpperdirKey                  = AnnotationSnapshotterPrefix + "overlay.upperdir"
	AnnotationSnapshotRefDir                       = AnnotationSnapshotterPrefix + "overlay.refdir"
	AnnotationSnapshotRef                          = "containerd.io/snapshot.ref"
	AnnotationSnapshotterHostLayerMountDigest      = AnnotationSnapshotterPrefix + "layermount.digest"
	AnnotationSnapshotterHostLayerMountHostPath    = AnnotationSnapshotterPrefix + "layermount.hostpath"
	AnnotationSnapshotterHostLayerMountParent      = AnnotationSnapshotterPrefix + "layermount.parent"
	AnnotationSnapshotterCustomUsage               = AnnotationSnapshotterPrefix + "custom-usage"
	AnnotationSnapshotterTargetManifestDigestLabel = AnnotationSnapshotterPrefix + "cri.manifest-digest"

	AnnotationImageConfigID = AnnotationSnapshotterPrefix + "image-config-id"
)

const (
	AnnotationContentDirectDigest = "containerd.io/content/direct-digest"
)

const (
	AnnotationCubeContainerName = "cube.container.name"

	AnnotationCubeSandboxImageID = "cube.container.sandbox.image.id"
)

const (
	AnnotationCubeletInternalPrefix = "cubelet.internal.param/"

	AnnotationCubeletInternalRuntimePath = AnnotationCubeletInternalPrefix + "runtime-path"
)

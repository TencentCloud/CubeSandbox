// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	volumepluginv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/volumeplugin/v1"
)

func TestValidateXFSS3PauseResources(t *testing.T) {
	rootMount := []*cubeboxv1.VolumeMounts{{Name: "root", ContainerPath: "/"}}
	rootfs := &cubeboxv1.Volume{Name: "root", VolumeSource: &cubeboxv1.VolumeSource{
		EmptyDir: &cubeboxv1.EmptyDirVolumeSource{SizeLimit: "32Gi"}}}

	plain := newRunningCommitSandboxForTest([]*cubeboxv1.Volume{rootfs}, rootMount)
	require.NoError(t, validateXFSS3PauseResources(plain))

	declaredHostMount := newRunningCommitSandboxForTest([]*cubeboxv1.Volume{rootfs}, rootMount)
	declaredHostMount.Annotations = map[string]string{"host-mount": `[{"hostPath":"/var/lib/data","mountPath":"/data"}]`}
	require.ErrorContains(t, validateXFSS3PauseResources(declaredHostMount), "host mounts")

	hostDir := newRunningCommitSandboxForTest([]*cubeboxv1.Volume{rootfs, {Name: "h", VolumeSource: &cubeboxv1.VolumeSource{
		HostDirVolumes: &cubeboxv1.HostDirVolumeSources{VolumeSources: []*cubeboxv1.HostDirSource{{Name: "h", HostPath: "/var/lib/x"}}}}}},
		append(rootMount, &cubeboxv1.VolumeMounts{Name: "h", ContainerPath: "/x"}))
	require.ErrorContains(t, validateXFSS3PauseResources(hostDir), "host")

	plugin := newRunningCommitSandboxForTest([]*cubeboxv1.Volume{rootfs, {Name: "data", VolumeSource: &cubeboxv1.VolumeSource{
		PluginVolume: &volumepluginv1.PluginVolumeSource{Driver: "s3"}}}},
		append(rootMount, &cubeboxv1.VolumeMounts{Name: "data", ContainerPath: "/data"}))
	require.ErrorContains(t, validateXFSS3PauseResources(plugin), "plugin volume")

	twoDisks := newRunningCommitSandboxForTest([]*cubeboxv1.Volume{rootfs, {Name: "scratch", VolumeSource: &cubeboxv1.VolumeSource{
		EmptyDir: &cubeboxv1.EmptyDirVolumeSource{SizeLimit: "1Gi"}}}},
		append(rootMount, &cubeboxv1.VolumeMounts{Name: "scratch", ContainerPath: "/scratch"}))
	require.ErrorContains(t, validateXFSS3PauseResources(twoDisks), "only the rootfs disk")

	memoryTmp := newRunningCommitSandboxForTest([]*cubeboxv1.Volume{rootfs, {Name: "tmp", VolumeSource: &cubeboxv1.VolumeSource{
		EmptyDir: &cubeboxv1.EmptyDirVolumeSource{Medium: cubeboxv1.StorageMedium_StorageMediumMemory}}}},
		append(rootMount, &cubeboxv1.VolumeMounts{Name: "tmp", ContainerPath: "/tmp"}))
	require.NoError(t, validateXFSS3PauseResources(memoryTmp), "a memory-backed empty_dir lives in guest memory")

	readOnlyPmem := newRunningCommitSandboxForTest([]*cubeboxv1.Volume{rootfs}, rootMount)
	readOnlyPmem.Annotations = map[string]string{constants.AnnotationPmem: `[{"id":"pmem-cubebox-image-0","file":"/img.ext4","fs_type":"ext4","discard_writes":true}]`}
	require.NoError(t, validateXFSS3PauseResources(readOnlyPmem))

	writablePmem := newRunningCommitSandboxForTest([]*cubeboxv1.Volume{rootfs}, rootMount)
	writablePmem.Annotations = map[string]string{constants.AnnotationPmem: `[{"id":"pmem-user","file":"/data/u.ext4","fs_type":"ext4"}]`}
	require.ErrorContains(t, validateXFSS3PauseResources(writablePmem), "writable pmem")
}

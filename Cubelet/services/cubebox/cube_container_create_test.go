// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	cubeconfig "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/disk"
	localnetfile "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/netfile"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/virtiofs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/volumefile"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/services/images"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

func TestIsImageStorageMediaType(t *testing.T) {
	tests := []struct {
		name      string
		container *cubebox.ContainerConfig
		mediaType cubeimages.ImageStorageMediaType
		want      bool
	}{
		{
			name: "docker image type",
			container: &cubebox.ContainerConfig{
				Image: &cubeimages.ImageSpec{
					StorageMedia: cubeimages.ImageStorageMediaType_docker.String(),
				},
			},
			mediaType: cubeimages.ImageStorageMediaType_docker,
			want:      true,
		},
		{
			name: "ext4 image type",
			container: &cubebox.ContainerConfig{
				Image: &cubeimages.ImageSpec{
					StorageMedia: cubeimages.ImageStorageMediaType_ext4.String(),
				},
			},
			mediaType: cubeimages.ImageStorageMediaType_ext4,
			want:      true,
		},
		{
			name: "empty storage media defaults to docker",
			container: &cubebox.ContainerConfig{
				Image: &cubeimages.ImageSpec{
					StorageMedia: "",
				},
			},
			mediaType: cubeimages.ImageStorageMediaType_docker,
			want:      true,
		},
		{
			name: "mismatched type",
			container: &cubebox.ContainerConfig{
				Image: &cubeimages.ImageSpec{
					StorageMedia: cubeimages.ImageStorageMediaType_ext4.String(),
				},
			},
			mediaType: cubeimages.ImageStorageMediaType_docker,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isImageStorageMediaType(tt.container, tt.mediaType)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGetMountOptions(t *testing.T) {
	tests := []struct {
		name  string
		mount *cubebox.VolumeMounts
		want  []string
	}{
		{
			name: "readonly with private propagation",
			mount: &cubebox.VolumeMounts{
				Readonly:    true,
				Propagation: cubebox.MountPropagation_PROPAGATION_PRIVATE,
			},
			want: []string{constants.MountOptBindRO, constants.MountPropagationRprivate, "ro"},
		},
		{
			name: "readwrite with bidirectional propagation",
			mount: &cubebox.VolumeMounts{
				Readonly:    false,
				Propagation: cubebox.MountPropagation_PROPAGATION_BIDIRECTIONAL,
			},
			want: []string{constants.MountOptBindRO, constants.MountPropagationRShared, "rw"},
		},
		{
			name: "readwrite with host to container propagation",
			mount: &cubebox.VolumeMounts{
				Readonly:    false,
				Propagation: cubebox.MountPropagation_PROPAGATION_HOST_TO_CONTAINER,
			},
			want: []string{constants.MountOptBindRO, constants.MountPropagationRSlave, "rw"},
		},
		{
			name: "readonly with default propagation",
			mount: &cubebox.VolumeMounts{
				Readonly:    true,
				Propagation: 999,
			},
			want: []string{constants.MountOptBindRO, constants.MountPropagationRprivate, "ro"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getMountOptions(tt.mount)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestStoreNumaQueues(t *testing.T) {
	tests := []struct {
		name               string
		opts               *workflow.CreateContext
		expectedNode       int32
		expectedQueues     int64
		expectedLabelValue string
	}{
		{
			name: "with numa node 1",
			opts: &workflow.CreateContext{
				BaseWorkflowInfo: workflow.BaseWorkflowInfo{
					NumaNode: 1,
				},
			},
			expectedNode:       1,
			expectedQueues:     0,
			expectedLabelValue: "1",
		},
		{
			name: "with numa node 0 and storage queues",
			opts: &workflow.CreateContext{
				BaseWorkflowInfo: workflow.BaseWorkflowInfo{
					NumaNode: 0,
				},
				StorageInfo: &storage.StorageInfo{
					CubePCIDiskInfo: &disk.CubePCIDiskInfo{
						Queues: 4,
					},
				},
			},
			expectedNode:       0,
			expectedQueues:     4,
			expectedLabelValue: "0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			cubebox := &cubeboxstore.CubeBox{
				Metadata: cubeboxstore.Metadata{
					Labels: make(map[string]string),
				},
			}

			l := &local{}
			l.storeNumaQueues(ctx, cubebox, tt.opts)

			assert.Equal(t, tt.expectedNode, cubebox.NumaNode)
			assert.Equal(t, tt.expectedQueues, cubebox.Queues)
			assert.Equal(t, tt.expectedLabelValue, cubebox.Labels[constants.LabelNumaNode])
		})
	}
}

func TestSandboxDNSServersFromContainers(t *testing.T) {
	tests := []struct {
		name       string
		defaultDNS []string
		req        *cubebox.RunCubeSandboxRequest
		want       []string
		wantErr    bool
	}{
		{
			name: "aggregate unique dns servers in order",
			req: &cubebox.RunCubeSandboxRequest{
				Containers: []*cubebox.ContainerConfig{
					{DnsConfig: &cubebox.DNSConfig{Servers: []string{"8.8.8.8", " 1.1.1.1 "}}},
					{DnsConfig: &cubebox.DNSConfig{Servers: []string{"1.1.1.1", "9.9.9.9"}}},
				},
			},
			want: []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"},
		},
		{
			name:       "fallback to cubelet default dns entries",
			defaultDNS: []string{"1.1.1.1", "9.9.9.9"},
			req: &cubebox.RunCubeSandboxRequest{
				Containers: []*cubebox.ContainerConfig{
					{DnsConfig: &cubebox.DNSConfig{Servers: []string{"", " "}}},
				},
			},
			want: []string{"1.1.1.1", "9.9.9.9"},
		},
		{
			name: "fallback to hardcoded dns server when config empty",
			req: &cubebox.RunCubeSandboxRequest{
				Containers: []*cubebox.ContainerConfig{{}},
			},
			want: []string{"119.29.29.29"},
		},
		{
			name: "reject invalid dns server",
			req: &cubebox.RunCubeSandboxRequest{
				Containers: []*cubebox.ContainerConfig{
					{DnsConfig: &cubebox.DNSConfig{Servers: []string{"not-an-ip"}}},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Init("", true)
			require.NoError(t, err)
			config.GetCommon().DefaultDNSServers = append([]string(nil), tt.defaultDNS...)
			got, err := sandboxDNSServersFromContainers(tt.req)
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAppendExt4NetfileMounts(t *testing.T) {
	rootPath := t.TempDir()
	containerName := "app"
	netfileDir := filepath.Join(rootPath, containerName)
	cnfs := &localnetfile.CubeboxNetfile{
		RootPath: rootPath,
		ContainerNetfiles: map[string]localnetfile.ContainerNetfile{
			containerName: {
				HostDirPath: netfileDir,
				Files: map[string]localnetfile.FileContent{
					"/etc/resolv.conf": {
						Path:    "/etc/resolv.conf",
						Content: []byte("nameserver 1.1.1.1\n"),
					},
					"/etc/hosts": {
						Path:    "/etc/hosts",
						Content: []byte("127.0.0.1 localhost\n"),
					},
					"/etc/hostname": {
						Path:    "/etc/hostname",
						Content: []byte("sandbox"),
					},
				},
			},
		},
	}

	mountsConfig := &virtiofs.CubeRootfsInfo{
		PmemFile: "/pmem/rootfs.ext4",
		Mounts: []virtiofs.CubeRootfsMount{
			{
				ContainerDest:  "/data",
				VirtiofsSource: "volume-data",
			},
		},
	}
	flowOpts := &workflow.CreateContext{
		NetFile: cnfs,
	}

	appendExt4NetfileMounts(mountsConfig, flowOpts, containerName)

	assert.Equal(t, "/pmem/rootfs.ext4", mountsConfig.PmemFile)
	assert.Len(t, mountsConfig.Mounts, 4)
	assert.Contains(t, mountsConfig.Mounts, virtiofs.CubeRootfsMount{
		HostSource:     filepath.Clean(filepath.Join(netfileDir, "/etc/resolv.conf")),
		VirtiofsSource: filepath.Clean(filepath.Join(filepath.Base(rootPath), containerName, "/etc/resolv.conf")),
		ContainerDest:  "/etc/resolv.conf",
		Type:           constants.MountTypeBind,
		Options:        []string{constants.MountOptBindRO, constants.MountOptReadOnly},
	})
	assert.Contains(t, mountsConfig.Mounts, virtiofs.CubeRootfsMount{
		HostSource:     filepath.Clean(filepath.Join(netfileDir, "/etc/hosts")),
		VirtiofsSource: filepath.Clean(filepath.Join(filepath.Base(rootPath), containerName, "/etc/hosts")),
		ContainerDest:  "/etc/hosts",
		Type:           constants.MountTypeBind,
		Options:        []string{constants.MountOptBindRO, constants.MountOptReadOnly},
	})
	assert.Contains(t, mountsConfig.Mounts, virtiofs.CubeRootfsMount{
		HostSource:     filepath.Clean(filepath.Join(netfileDir, "/etc/hostname")),
		VirtiofsSource: filepath.Clean(filepath.Join(filepath.Base(rootPath), containerName, "/etc/hostname")),
		ContainerDest:  "/etc/hostname",
		Type:           constants.MountTypeBind,
		Options:        []string{constants.MountOptBindRO, constants.MountOptReadOnly},
	})
}

func TestAppendExt4NetfileMountsNoopWithoutNetfile(t *testing.T) {
	mountsConfig := &virtiofs.CubeRootfsInfo{
		PmemFile: "/pmem/rootfs.ext4",
	}

	appendExt4NetfileMounts(mountsConfig, &workflow.CreateContext{}, "app")
	appendExt4NetfileMounts(mountsConfig, nil, "app")

	assert.Equal(t, "/pmem/rootfs.ext4", mountsConfig.PmemFile)
	assert.Empty(t, mountsConfig.Mounts)
}

func TestWithRuntimePathOpt(t *testing.T) {
	tests := []struct {
		name    string
		cubebox *cubeboxstore.CubeBox
	}{
		{
			name: "use oci runtime path",
			cubebox: &cubeboxstore.CubeBox{
				OciRuntime: &cubeconfig.Runtime{
					Path: "/custom/runtime/path",
				},
			},
		},
		{
			name: "use local run template component path",
			cubebox: &cubeboxstore.CubeBox{
				OciRuntime: &cubeconfig.Runtime{
					Path: "",
				},
				LocalRunTemplate: &templatetypes.LocalRunTemplate{
					Components: map[string]templatetypes.LocalComponent{
						templatetypes.CubeComponentCubeShim: {
							Component: templatetypes.MachineComponent{
								Path: "/template/shim/path",
							},
						},
					},
				},
			},
		},
		{
			name: "use annotation runtime path - highest priority",
			cubebox: &cubeboxstore.CubeBox{
				Metadata: cubeboxstore.Metadata{
					Annotations: map[string]string{
						constants.AnnotationCubeletInternalRuntimePath: "/annotation/runtime/path",
					},
				},
				OciRuntime: &cubeconfig.Runtime{
					Path: "/custom/runtime/path",
				},
				LocalRunTemplate: &templatetypes.LocalRunTemplate{
					Components: map[string]templatetypes.LocalComponent{
						templatetypes.CubeComponentCubeShim: {
							Component: templatetypes.MachineComponent{
								Path: "/template/shim/path",
							},
						},
					},
				},
			},
		},
		{
			name: "no runtime path set",
			cubebox: &cubeboxstore.CubeBox{
				OciRuntime: &cubeconfig.Runtime{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			assert.NotPanics(t, func() {
				_ = withRuntimePathOpt(tt.cubebox)
			})
		})
	}
}

func TestGenerateContainerID(t *testing.T) {
	tests := []struct {
		name            string
		flowOpts        *workflow.CreateContext
		index           int
		checkSandboxID  bool
		checkTemplateID bool
	}{
		{
			name: "first container (pod) with sandbox ID",
			flowOpts: &workflow.CreateContext{
				BaseWorkflowInfo: workflow.BaseWorkflowInfo{
					SandboxID: "sandbox-12345",
				},
			},
			index:          0,
			checkSandboxID: true,
		},
		{
			name: "second container generates random ID",
			flowOpts: &workflow.CreateContext{
				BaseWorkflowInfo: workflow.BaseWorkflowInfo{
					SandboxID: "sandbox-12345",
				},
			},
			index:          1,
			checkSandboxID: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			l := &local{}

			ctxResult, cid := l.generateContainerID(ctx, tt.flowOpts, tt.index)

			assert.NotNil(t, ctxResult)

			assert.NotEmpty(t, cid)

			if tt.checkSandboxID {
				assert.Equal(t, tt.flowOpts.GetSandboxID(), cid)
			}
		})
	}
}

func TestPrepareSandboxPathVolume(t *testing.T) {
	tests := []struct {
		name          string
		containerReq  *cubebox.ContainerConfig
		realReq       *cubebox.RunCubeSandboxRequest
		expectedCount int
		validateMount func(*testing.T, []specs.Mount)
	}{
		{
			name: "no sandbox path volumes",
			containerReq: &cubebox.ContainerConfig{
				VolumeMounts: []*cubebox.VolumeMounts{},
			},
			realReq: &cubebox.RunCubeSandboxRequest{
				Volumes: []*cubebox.Volume{},
			},
			expectedCount: 0,
		},
		{
			name: "cgroup type sandbox path",
			containerReq: &cubebox.ContainerConfig{
				VolumeMounts: []*cubebox.VolumeMounts{
					{
						Name:          "cgroup-vol",
						ContainerPath: "/sys/fs/cgroup/test",
					},
				},
			},
			realReq: &cubebox.RunCubeSandboxRequest{
				Volumes: []*cubebox.Volume{
					{
						Name: "cgroup-vol",
						VolumeSource: &cubebox.VolumeSource{
							SandboxPath: &cubebox.SandboxPathVolumeSource{
								Type: cubebox.SandboxPathType_Cgroup.String(),
								Path: "/test",
							},
						},
					},
				},
			},
			expectedCount: 1,
			validateMount: func(t *testing.T, mounts []specs.Mount) {
				assert.Equal(t, constants.MountTypeBind, mounts[0].Type)
				assert.Equal(t, filepath.Join("/sys/fs/cgroup", "test"), mounts[0].Source)
				assert.Contains(t, mounts[0].Options, constants.MountOptReadOnly)
			},
		},
		{
			name: "directory type sandbox path",
			containerReq: &cubebox.ContainerConfig{
				VolumeMounts: []*cubebox.VolumeMounts{
					{
						Name:          "dir-vol",
						ContainerPath: "/mnt/data",
						Readonly:      false,
					},
				},
			},
			realReq: &cubebox.RunCubeSandboxRequest{
				Volumes: []*cubebox.Volume{
					{
						Name: "dir-vol",
						VolumeSource: &cubebox.VolumeSource{
							SandboxPath: &cubebox.SandboxPathVolumeSource{
								Type: cubebox.SandboxPathType_Directory.String(),
								Path: "/host/data",
							},
						},
					},
				},
			},
			expectedCount: 1,
			validateMount: func(t *testing.T, mounts []specs.Mount) {
				assert.Equal(t, constants.MountTypeBind, mounts[0].Type)
				assert.Equal(t, "/host/data", mounts[0].Source)
				assert.Equal(t, "/mnt/data", mounts[0].Destination)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			l := &local{}

			mounts := l.prepareSandboxPathVolume(ctx, tt.containerReq, tt.realReq)

			assert.Len(t, mounts, tt.expectedCount)

			if tt.validateMount != nil && len(mounts) > 0 {
				tt.validateMount(t, mounts)
			}
		})
	}
}

func TestPrepareVolumeAnnotations(t *testing.T) {
	tests := []struct {
		name      string
		opts      *workflow.CreateContext
		expectNil bool
	}{
		{
			name: "no storage info",
			opts: &workflow.CreateContext{
				StorageInfo: nil,
			},
			expectNil: true,
		},
		{
			name: "storage info with no PCI disks",
			opts: &workflow.CreateContext{
				StorageInfo: &storage.StorageInfo{},
			},
			expectNil: true,
		},
		{
			name: "storage info with system disk",
			opts: &workflow.CreateContext{
				StorageInfo: &storage.StorageInfo{
					CubePCISystemDiskInfo: &disk.CubePCISystemDiskInfo{
						PCISystemDisk: disk.CubePCIDisk{
							ID: "sys-disk-1",
						},
					},
				},
			},
			expectNil: false,
		},
		{
			name: "storage info with data disks",
			opts: &workflow.CreateContext{
				StorageInfo: &storage.StorageInfo{
					CubePCIDiskInfo: &disk.CubePCIDiskInfo{
						PCIDisks: []disk.CubePCIDisk{
							{
								ID: "data-disk-1",
							},
						},
					},
				},
			},
			expectNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			l := &local{}

			opts, err := l.prepareVolumeAnnotations(ctx, tt.opts)

			assert.NoError(t, err)

			if tt.expectNil {
				assert.Nil(t, opts)
			} else {
				assert.NotNil(t, opts)
				assert.Greater(t, len(opts), 0)
			}
		})
	}
}

func TestGetSandboxRuntime(t *testing.T) {
	defaultRuntime := cubeconfig.Runtime{
		Type:        "io.containerd.cube.v1",
		Path:        "/usr/bin/containerd-shim-cube",
		Snapshotter: "overlayfs",
	}

	customRuntime := cubeconfig.Runtime{
		Type:        "io.containerd.runc.v2",
		Path:        "/usr/bin/containerd-shim-runc-v2",
		Snapshotter: "overlayfs",
	}

	tests := []struct {
		name          string
		local         *local
		req           *cubebox.RunCubeSandboxRequest
		expectedType  string
		expectError   bool
		errorContains string
	}{
		{
			name: "use default runtime when not specified",
			local: &local{
				config: &CubeConfig{
					DefaultRuntimeName: "cube",
					Runtimes: map[string]cubeconfig.Runtime{
						"cube": defaultRuntime,
					},
				},
			},
			req: &cubebox.RunCubeSandboxRequest{
				RuntimeHandler: "",
			},
			expectedType: "io.containerd.cube.v1",
			expectError:  false,
		},
		{
			name: "use specified runtime handler",
			local: &local{
				config: &CubeConfig{
					DefaultRuntimeName: "cube",
					Runtimes: map[string]cubeconfig.Runtime{
						"cube": defaultRuntime,
						"runc": customRuntime,
					},
				},
			},
			req: &cubebox.RunCubeSandboxRequest{
				RuntimeHandler: "runc",
			},
			expectedType: "io.containerd.runc.v2",
			expectError:  false,
		},
		{
			name: "error when runtime not found",
			local: &local{
				config: &CubeConfig{
					DefaultRuntimeName: "cube",
					Runtimes: map[string]cubeconfig.Runtime{
						"cube": defaultRuntime,
					},
				},
			},
			req: &cubebox.RunCubeSandboxRequest{
				RuntimeHandler: "unknown",
			},
			expectError:   true,
			errorContains: "no runtime for",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime, err := tt.local.getSandboxRuntime(tt.req)

			if tt.expectError {
				assert.Error(t, err)
				if tt.errorContains != "" {
					assert.Contains(t, err.Error(), tt.errorContains)
				}
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectedType, runtime.Type)
			}
		})
	}
}

func TestDeepCopyStringMap(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]string
		validate func(*testing.T, map[string]string, map[string]string)
	}{
		{
			name:  "nil map",
			input: nil,
			validate: func(t *testing.T, original, copy map[string]string) {
				assert.Nil(t, copy)
			},
		},
		{
			name:  "empty map",
			input: map[string]string{},
			validate: func(t *testing.T, original, copy map[string]string) {
				assert.NotNil(t, copy)
				assert.Len(t, copy, 0)
			},
		},
		{
			name: "non-empty map",
			input: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
			validate: func(t *testing.T, original, copy map[string]string) {
				assert.Equal(t, original, copy)

				copy["key3"] = "value3"
				assert.NotContains(t, original, "key3")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := deepCopyStringMap(tt.input)
			tt.validate(t, tt.input, result)
		})
	}
}

func TestPrepareImagePmems(t *testing.T) {

	tmpFile := t.TempDir() + "/test-image.ext4"
	err := os.WriteFile(tmpFile, []byte("test content"), 0644)
	require.NoError(t, err)

	tests := []struct {
		name         string
		rootfsConfig []*virtiofs.CubeRootfsInfo
		expectPmems  int
		validate     func(*testing.T, *specs.Spec)
	}{
		{
			name:         "no pmem files",
			rootfsConfig: []*virtiofs.CubeRootfsInfo{},
			expectPmems:  0,
		},
		{
			name: "one pmem file",
			rootfsConfig: []*virtiofs.CubeRootfsInfo{
				{
					PmemFile: tmpFile,
				},
			},
			expectPmems: 1,
			validate: func(t *testing.T, spec *specs.Spec) {
				assert.Contains(t, spec.Annotations, constants.AnnotationPmem)
			},
		},
		{
			name: "multiple pmem files",
			rootfsConfig: []*virtiofs.CubeRootfsInfo{
				{PmemFile: tmpFile},
				{PmemFile: tmpFile},
			},
			expectPmems: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			spec := &specs.Spec{
				Annotations: make(map[string]string),
			}
			container := &containers.Container{}

			opt := prepareImagePmems(tt.rootfsConfig)
			err := opt(ctx, nil, container, spec)

			if tt.expectPmems == 0 {
				assert.NoError(t, err)
				assert.NotContains(t, spec.Annotations, constants.AnnotationPmem)
			} else {
				assert.NoError(t, err)
				if tt.validate != nil {
					tt.validate(t, spec)
				}
			}
		})
	}
}

func TestSetCgroup(t *testing.T) {
	tests := []struct {
		name  string
		pid   uint32
		group string
	}{
		{
			name:  "valid pid and group",
			pid:   1234,
			group: "/sys/fs/cgroup/test",
		},
		{
			name:  "zero pid",
			pid:   0,
			group: "/sys/fs/cgroup/test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			assert.NotPanics(t, func() {
				setCgroup(ctx, tt.pid, tt.group)
			})
		})
	}
}

func TestPrepareVolumePmems(t *testing.T) {

	tmpFile := t.TempDir() + "/test-volume.ext4"
	err := os.WriteFile(tmpFile, []byte("test volume content"), 0644)
	require.NoError(t, err)

	tests := []struct {
		name        string
		flowOpts    *workflow.CreateContext
		expectPmems bool
	}{
		{
			name: "no volume info",
			flowOpts: &workflow.CreateContext{
				VolumeInfo: nil,
			},
			expectPmems: false,
		},
		{
			name: "volume info with ext4 volumes",
			flowOpts: &workflow.CreateContext{
				VolumeInfo: &images.Info{
					Volumes: map[string]*images.BackendFileInfo{
						"vol1": {
							FilePath: tmpFile,
							FileType: volumefile.FtLangExt4,
						},
					},
				},
			},
			expectPmems: true,
		},
		{
			name: "volume info with non-ext4 volumes",
			flowOpts: &workflow.CreateContext{
				VolumeInfo: &images.Info{
					Volumes: map[string]*images.BackendFileInfo{
						"vol1": {
							FilePath: tmpFile,
							FileType: volumefile.FtLang,
						},
					},
				},
			},
			expectPmems: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			spec := &specs.Spec{
				Annotations: make(map[string]string),
			}
			container := &containers.Container{}

			opt := prepareVolumePmems(tt.flowOpts)
			err := opt(ctx, nil, container, spec)

			assert.NoError(t, err)

			if tt.expectPmems {
				assert.Contains(t, spec.Annotations, constants.AnnotationPmem)
			} else {
				assert.NotContains(t, spec.Annotations, constants.AnnotationPmem)
			}
		})
	}
}

func TestGenGeneralContainerSpecOpt(t *testing.T) {
	tests := []struct {
		name         string
		containerReq *cubebox.ContainerConfig
		ci           *cubeboxstore.Container
		imageConfig  *imagespec.ImageConfig
		validate     func(*testing.T, []oci.SpecOpts)
	}{
		{
			name: "basic container config",
			containerReq: &cubebox.ContainerConfig{
				WorkingDir: "/app",
				Envs: []*cubebox.KeyValue{
					{Key: "ENV1", Value: "value1"},
				},
			},
			ci: &cubeboxstore.Container{
				Metadata: cubeboxstore.Metadata{
					ID: "test-container",
				},
			},
			imageConfig: &imagespec.ImageConfig{
				WorkingDir: "/default",
			},
			validate: func(t *testing.T, opts []oci.SpecOpts) {
				assert.Greater(t, len(opts), 0)
			},
		},
		{
			name: "privileged container",
			containerReq: &cubebox.ContainerConfig{
				SecurityContext: &cubebox.ContainerSecurityContext{
					Privileged: true,
				},
			},
			ci: &cubeboxstore.Container{
				Metadata: cubeboxstore.Metadata{
					ID: "test-privileged",
				},
			},
			imageConfig: &imagespec.ImageConfig{},
			validate: func(t *testing.T, opts []oci.SpecOpts) {
				assert.Greater(t, len(opts), 0)
			},
		},
		{
			name: "container with rlimit",
			containerReq: &cubebox.ContainerConfig{
				RLimit: &cubebox.RLimit{
					NoFile: 65536,
				},
			},
			ci: &cubeboxstore.Container{
				Metadata: cubeboxstore.Metadata{
					ID: "test-rlimit",
				},
			},
			imageConfig: &imagespec.ImageConfig{},
			validate: func(t *testing.T, opts []oci.SpecOpts) {
				assert.Greater(t, len(opts), 0)
			},
		},
		{
			name: "container with no new privileges",
			containerReq: &cubebox.ContainerConfig{
				SecurityContext: &cubebox.ContainerSecurityContext{
					NoNewPrivs: true,
				},
			},
			ci: &cubeboxstore.Container{
				Metadata: cubeboxstore.Metadata{
					ID: "test-no-new-privs",
				},
			},
			imageConfig: &imagespec.ImageConfig{},
			validate: func(t *testing.T, opts []oci.SpecOpts) {
				assert.Greater(t, len(opts), 0)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			opts := genGeneralContainerSpecOpt(ctx, tt.containerReq, tt.ci, tt.imageConfig)

			assert.NotNil(t, opts)
			if tt.validate != nil {
				tt.validate(t, opts)
			}
		})
	}
}

func TestGenContainerLabels(t *testing.T) {
	tests := []struct {
		name     string
		ci       *cubeboxstore.Container
		req      *cubebox.RunCubeSandboxRequest
		validate func(*testing.T, map[string]string)
	}{
		{
			name: "pod container with labels",
			ci: &cubeboxstore.Container{
				Metadata: cubeboxstore.Metadata{
					SandboxID: "sandbox-123",
				},
				IsPod: true,
			},
			req: &cubebox.RunCubeSandboxRequest{
				Labels: map[string]string{
					"app":  "test-app",
					"tier": "backend",
				},
			},
			validate: func(t *testing.T, labels map[string]string) {
				assert.Equal(t, constants.ContainerTypeSandBox, labels[constants.ContainerType])
				assert.Equal(t, "test-app", labels["app"])
				assert.Equal(t, "backend", labels["tier"])
				assert.Equal(t, "sandbox-123", labels[constants.SandboxID])
			},
		},
		{
			name: "non-pod container",
			ci: &cubeboxstore.Container{
				Metadata: cubeboxstore.Metadata{
					SandboxID: "sandbox-456",
				},
				IsPod: false,
			},
			req: &cubebox.RunCubeSandboxRequest{
				Labels: map[string]string{
					"app": "test-app",
				},
			},
			validate: func(t *testing.T, labels map[string]string) {
				assert.Equal(t, constants.ContainerTypeContainer, labels[constants.ContainerType])
				assert.Equal(t, "sandbox-456", labels[constants.SandboxID])
				assert.NotContains(t, labels, "app")
			},
		},
		{
			name: "pod without legacy identity labels",
			ci: &cubeboxstore.Container{
				Metadata: cubeboxstore.Metadata{
					SandboxID: "sandbox-789",
				},
				IsPod: true,
			},
			req: &cubebox.RunCubeSandboxRequest{
				Labels:      map[string]string{},
				Annotations: map[string]string{},
			},
			validate: func(t *testing.T, labels map[string]string) {
				assert.Equal(t, constants.ContainerTypeSandBox, labels[constants.ContainerType])
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := &local{}
			labels := l.genContainerLabels(tt.ci, tt.req)

			assert.NotNil(t, labels)
			if tt.validate != nil {
				tt.validate(t, labels)
			}
		})
	}
}

func TestTransformError(t *testing.T) {
	tests := []struct {
		name        string
		inputErr    error
		expectNil   bool
		expectError bool
	}{
		{
			name:        "nil error",
			inputErr:    nil,
			expectNil:   true,
			expectError: false,
		},
		{
			name:        "regular error",
			inputErr:    fmt.Errorf("some error"),
			expectNil:   false,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := transformError(tt.inputErr)

			if tt.expectNil {
				assert.Nil(t, result)
			} else if tt.expectError {
				assert.Error(t, result)
			}
		})
	}
}

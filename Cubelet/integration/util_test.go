// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/containerd/containerd/api/services/tasks/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/platforms"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

func IsCube() bool {
	return os.Getenv("RUNTIME") == "cube"
}

func SimpleCubeboxConfig() *cubebox.RunCubeSandboxRequest {
	req := &cubebox.RunCubeSandboxRequest{
		RequestID: uuid.New().String(),
		Volumes: []*cubebox.Volume{
			{
				Name: "tmp",
				VolumeSource: &cubebox.VolumeSource{
					EmptyDir: &cubebox.EmptyDirVolumeSource{
						SizeLimit: "512Mi",
					},
				},
			},
			{
				Name: "tmpfs",
				VolumeSource: &cubebox.VolumeSource{
					EmptyDir: &cubebox.EmptyDirVolumeSource{
						Medium:    1,
						SizeLimit: "16Mi",
					},
				},
			},
		},
		Containers: []*cubebox.ContainerConfig{
			{
				Image: &images.ImageSpec{
					Image: "busybox",
				},
				WorkingDir: "/",
				Command: []string{
					"/bin/top",
				},
				Envs: []*cubebox.KeyValue{
					{
						Key:   "TERM",
						Value: "xterm",
					},
				},
				DnsConfig: &cubebox.DNSConfig{
					Servers: []string{"8.8.8.8"},
				},
				Sysctls: map[string]string{
					"kernel.shm_rmid_forced": "0",
					"net.ipv4.ip_forward":    "1",
					"net.core.somaxconn":     "256",
				},
				Syscalls: []*cubebox.SysCall{
					{
						Names:  []string{"chroot", "chmod"},
						Action: "SCMP_ACT_ERRNO",
					},
					{
						Names:  []string{"ptrace"},
						Action: "SCMP_ACT_ALLOW",
					},
				},
				RLimit: &cubebox.RLimit{
					NoFile: 10000,
				},
				Resources: &cubebox.Resource{
					Cpu: "100m",
					Mem: "128Mi",
				},
				SecurityContext: &cubebox.ContainerSecurityContext{
					Capabilities: &cubebox.Capability{
						AddCapabilities:  []string{"CHOWN", "SYS_ADMIN", "NET_ADMIN", "NET_RAW", "NET_BROADCAST"},
						DropCapabilities: []string{"ALL"},
					},
					RunAsUser:      &cubebox.Int64Value{Value: 0},
					RunAsGroup:     &cubebox.Int64Value{Value: 0},
					ReadonlyRootfs: true,
				},
				Annotations: map[string]string{
					"cube_annotation_test_a": "a",
				},
			},
		},
		Annotations: map[string]string{},
	}
	if IsCube() {
		req.Containers[0].VolumeMounts = []*cubebox.VolumeMounts{
			{
				Name:          "tmp",
				ContainerPath: "/tmp",
			},
			{
				Name:          "tmpfs",
				ContainerPath: "/mytmpfs",
			},
		}
	}
	return req
}

func SimpleCubeboxConfigWithCleanup(t *testing.T) string {
	req := SimpleCubeboxConfig()
	sandboxID := CreateCubeboxSuccess(t, req)
	require.NotEmpty(t, sandboxID)
	t.Cleanup(func() {
		DestroyCubeboxSuccess(t, sandboxID)
	})
	return sandboxID
}

func StandardCubeboxConfig() *cubebox.RunCubeSandboxRequest {
	req := &cubebox.RunCubeSandboxRequest{
		RequestID: uuid.New().String(),
		Volumes: []*cubebox.Volume{
			{
				Name: "tmp",
				VolumeSource: &cubebox.VolumeSource{
					EmptyDir: &cubebox.EmptyDirVolumeSource{
						SizeLimit: "512Mi",
					},
				},
			},
			{
				Name: "tmpfs",
				VolumeSource: &cubebox.VolumeSource{
					EmptyDir: &cubebox.EmptyDirVolumeSource{
						Medium:    1,
						SizeLimit: "16Mi",
					},
				},
			},
		},
		Containers: []*cubebox.ContainerConfig{
			{
				Image: &images.ImageSpec{
					Image: "busybox",
				},
				WorkingDir: "/",
				Command: []string{
					"/bin/top",
				},
				Envs: []*cubebox.KeyValue{
					{
						Key:   "TERM",
						Value: "xterm",
					},
				},
				DnsConfig: &cubebox.DNSConfig{
					Servers: []string{"8.8.8.8"},
				},
				Sysctls: map[string]string{
					"kernel.shm_rmid_forced": "0",
					"net.ipv4.ip_forward":    "1",
					"net.core.somaxconn":     "256",
				},
				Syscalls: []*cubebox.SysCall{
					{
						Names:  []string{"chroot", "chmod"},
						Action: "SCMP_ACT_ERRNO",
					},
					{
						Names:  []string{"ptrace"},
						Action: "SCMP_ACT_ALLOW",
					},
				},
				RLimit: &cubebox.RLimit{
					NoFile: 10000,
				},
				Resources: &cubebox.Resource{
					Cpu: "100m",
					Mem: "128Mi",
				},
				SecurityContext: &cubebox.ContainerSecurityContext{
					Capabilities: &cubebox.Capability{
						AddCapabilities:  []string{"CHOWN", "SYS_ADMIN", "NET_ADMIN", "NET_RAW", "NET_BROADCAST"},
						DropCapabilities: []string{"ALL"},
					},
					RunAsUser:      &cubebox.Int64Value{Value: 0},
					RunAsGroup:     &cubebox.Int64Value{Value: 0},
					ReadonlyRootfs: true,
				},
				Annotations: map[string]string{
					"cube_annotation_test_a": "a",
				},
			},
			{
				Image: &images.ImageSpec{
					Image: "busybox",
				},
				WorkingDir: "/",
				Command: []string{
					"/bin/top",
				},
				Envs: []*cubebox.KeyValue{
					{
						Key:   "TERM",
						Value: "xterm",
					},
				},
				DnsConfig: &cubebox.DNSConfig{
					Servers: []string{"8.8.8.8"},
				},
				Sysctls: map[string]string{
					"kernel.shm_rmid_forced": "0",
					"net.ipv4.ip_forward":    "1",
					"net.core.somaxconn":     "256",
				},
				Syscalls: []*cubebox.SysCall{
					{
						Names:  []string{"chroot", "chmod"},
						Action: "SCMP_ACT_ERRNO",
					},
					{
						Names:  []string{"ptrace"},
						Action: "SCMP_ACT_ALLOW",
					},
				},
				RLimit: &cubebox.RLimit{
					NoFile: 10000,
				},
				Resources: &cubebox.Resource{
					Cpu: "100m",
					Mem: "128Mi",
				},
				SecurityContext: &cubebox.ContainerSecurityContext{
					Capabilities: &cubebox.Capability{
						AddCapabilities:  []string{"CHOWN", "SYS_ADMIN", "NET_ADMIN", "NET_RAW", "NET_BROADCAST"},
						DropCapabilities: []string{"ALL"},
					},
					RunAsUser:      &cubebox.Int64Value{Value: 0},
					RunAsGroup:     &cubebox.Int64Value{Value: 0},
					ReadonlyRootfs: true,
				},
				Annotations: map[string]string{
					"cube_annotation_test_a": "b",
				},
			},
		},
		Annotations: map[string]string{},
	}
	if IsCube() {
		req.Containers[0].VolumeMounts = []*cubebox.VolumeMounts{
			{
				Name:          "tmp",
				ContainerPath: "/tmp",
			},
			{
				Name:          "tmpfs",
				ContainerPath: "/mytmpfs",
			},
		}
		req.Containers[1].VolumeMounts = []*cubebox.VolumeMounts{
			{
				Name:          "tmp",
				ContainerPath: "/tmp",
			},
			{
				Name:          "tmpfs",
				ContainerPath: "/mytmpfs",
			},
		}
	}
	return req
}

func StandardCubeboxConfigWithCleanup(t *testing.T) string {
	req := StandardCubeboxConfig()
	sandboxID := CreateCubeboxSuccess(t, req)
	require.NotEmpty(t, sandboxID)
	t.Cleanup(func() {
		DestroyCubeboxSuccess(t, sandboxID)
	})
	return sandboxID
}

func ListCubebox(t *testing.T, req *cubebox.ListCubeSandboxRequest) []*cubebox.CubeSandbox {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	resp, err := cubeClient.List(ctx, req)
	if !assert.NoError(t, err) {
		return nil
	}
	return resp.GetItems()
}

func CreateCubeboxSuccess(t *testing.T, req *cubebox.RunCubeSandboxRequest) string {

	resp := CreateCubebox(t, req)
	if !assert.Equalf(t, resp.GetRet().GetRetCode(), errorcode.ErrorCode_Success,
		"create sandbox: %v, reqid: %v", resp.GetRet().GetRetMsg(), req.RequestID) {
		return ""
	}
	return resp.GetSandboxID()
}

func CreateCubebox(t *testing.T, req *cubebox.RunCubeSandboxRequest) *cubebox.RunCubeSandboxResponse {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	resp, err := cubeClient.Create(ctx, req)
	assert.NoError(t, err)
	return resp
}

func DestroyCubeboxSuccess(t *testing.T, sandboxID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	dReq := &cubebox.DestroyCubeSandboxRequest{
		RequestID: uuid.New().String(),
		SandboxID: sandboxID,
	}
	dResp, err := cubeClient.Destroy(ctx, dReq)
	require.NoError(t, err)
	assert.Equalf(t, dResp.GetRet().GetRetCode(), errorcode.ErrorCode_Success,
		"destroy sandbox: %v, reqid: %v", dResp.GetRet().GetRetMsg(), dResp.RequestID)
	assert.Equalf(t, sandboxID, dResp.GetSandboxID(),
		"destroy resp sandboxID should be equal to request")

	EnsureSandboxDeleted(t, sandboxID)
}

func CleanupCubeboxSuccess(t *testing.T, sandboxID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	dReq := &cubebox.DestroyCubeSandboxRequest{
		RequestID: uuid.New().String(),
		SandboxID: sandboxID,
	}
	dResp, err := cubeClient.Destroy(ctx, dReq)
	require.NoError(t, err)
	assert.Equalf(t, dResp.GetRet().GetRetCode(), errorcode.ErrorCode_Success,
		"destroy sandbox: %v, reqid: %v", dResp.GetRet().GetRetMsg(), dResp.RequestID)
	assert.Equalf(t, sandboxID, dResp.GetSandboxID(),
		"destroy resp sandboxID should be equal to request")
}

func CreateImage(t *testing.T, req *images.CreateImageRequest) *images.CreateImageRequestResponse {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := imageClient.CreateImage(ctx, req)
	assert.NoError(t, err)
	return resp
}

func DestroyImage(t *testing.T, req *images.DestroyImageRequest) *images.DestroyImageResponse {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, _ := imageClient.DestroyImage(ctx, req)

	return resp
}

func NewDefaultContainerdClient(t *testing.T) *containerd.Client {
	cntdClient, err := containerd.New("/data/cubelet/cubelet.sock",
		containerd.WithDefaultPlatform(platforms.Default()),
	)
	require.NoError(t, err, "new containerd client failed")
	return cntdClient
}

func EnsureSandboxDeleted(t *testing.T, id string) {
	ctr := NewDefaultContainerdClient(t)

	cntCtx := namespaces.WithNamespace(context.Background(), namespaces.Default)

	_, err := ctr.TaskService().Get(cntCtx, &tasks.GetRequest{
		ContainerID: id,
	})
	if err == nil {
		t.Errorf("task %s should be deleted", id)
	} else {
		err = errgrpc.ToNative(err)
		if !errdefs.IsNotFound(err) {
			t.Errorf("task %s should be deleted, but got error %v", id, err)
		}
	}

	taskStateDir := fmt.Sprintf("/data/cubelet/root/io.containerd.runtime.v2.task/default/%s", id)
	exist, _ := utils.DenExist(taskStateDir)
	assert.Falsef(t, exist, "rootfs %s should be deleted", taskStateDir)

	_, err = ctr.ContainerService().Get(cntCtx, id)
	if err == nil {
		t.Errorf("sandbox %s should be deleted", id)
	} else if !errdefs.IsNotFound(err) {
		t.Errorf("sandbox %s should be deleted, but got error %v", id, err)
	}

	if IsCube() {

		netfile := fmt.Sprintf("/data/cubelet/root/io.cubelet.internal.v1.cubebox/netfile/%s", id)
		exist, _ := utils.DenExist(netfile)
		assert.Falsef(t, exist, "netfile %s should be deleted", netfile)

		rootfs := fmt.Sprintf("/data/cubelet/state/io.cubelet.internal.v1.cubebox/rootfs/%s", id)
		exist, _ = utils.DenExist(rootfs)
		assert.Falsef(t, exist, "rootfs %s should be deleted", rootfs)

		cubeShared := fmt.Sprintf("/run/cube-containers/shared/sandboxes/%s", id)
		exist, _ = utils.DenExist(cubeShared)
		assert.Falsef(t, exist, "cube shared %s should be deleted", cubeShared)
	}
}

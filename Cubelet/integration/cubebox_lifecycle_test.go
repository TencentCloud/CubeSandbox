// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

func TestCubeboxCreateDestroy(t *testing.T) {
	id := CreateCubeboxSuccess(t, StandardCubeboxConfig())
	if id != "" {
		DestroyCubeboxSuccess(t, id)
	}
}

func TestCubeboxCreateTimeout(t *testing.T) {
	cfg := StandardCubeboxConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	resp, err := cubeClient.Create(ctx, cfg)
	assert.Error(t, err)

	if resp != nil && resp.SandboxID != "" {
		DestroyCubeboxSuccess(t, resp.SandboxID)
	}
}

func TestCubeboxCreateNil(t *testing.T) {
	resp := CreateCubebox(t, &cubebox.RunCubeSandboxRequest{})
	assert.NotEqual(t, errorcode.ErrorCode_Success, resp.GetRet().GetRetCode())

	if resp.SandboxID != "" {
		DestroyCubeboxSuccess(t, resp.SandboxID)
	}
}

func TestCubeboxCreateCmdNotFound(t *testing.T) {
	req := SimpleCubeboxConfig()
	req.GetContainers()[0].Command = []string{"notfound"}
	resp := CreateCubebox(t, req)
	assert.Equal(t, errorcode.ErrorCode_NewTaskFailed, resp.GetRet().GetRetCode())
	if resp.SandboxID != "" {
		DestroyCubeboxSuccess(t, resp.SandboxID)
	}
}

func TestCubeboxImmediateExit(t *testing.T) {
	req := SimpleCubeboxConfig()
	req.GetContainers()[0].Command = []string{"/bin/ls"}
	resp := CreateCubebox(t, req)
	assert.Equal(t, errorcode.ErrorCode_Success, resp.GetRet().GetRetCode())

	time.Sleep(1 * time.Second)

	list := ListCubebox(t, &cubebox.ListCubeSandboxRequest{
		Id: &resp.SandboxID,
	})
	require.Len(t, list, 1)

	assert.Equal(t, cubebox.ContainerState_CONTAINER_EXITED, list[0].GetContainers()[0].GetState())

	if resp.SandboxID != "" {
		DestroyCubeboxSuccess(t, resp.SandboxID)
	}
}

func TestCubeboxContainerImmediateExit(t *testing.T) {
	req := StandardCubeboxConfig()
	req.GetContainers()[1].Command = []string{"/bin/ls"}
	resp := CreateCubebox(t, req)
	assert.Equal(t, errorcode.ErrorCode_Success, resp.GetRet().GetRetCode())

	time.Sleep(1 * time.Second)

	list := ListCubebox(t, &cubebox.ListCubeSandboxRequest{
		Id: &resp.SandboxID,
	})
	require.Len(t, list, 1)

	fmt.Printf("%v", list[0].GetContainers())

	assert.Equal(t, cubebox.ContainerState_CONTAINER_EXITED, list[0].GetContainers()[1].GetState())

	if resp.SandboxID != "" {
		DestroyCubeboxSuccess(t, resp.SandboxID)
	}
}

func TestCubeboxCreateNoMountContainerPath(t *testing.T) {
	req := SimpleCubeboxConfig()
	req.GetContainers()[0].VolumeMounts = []*cubebox.VolumeMounts{
		{
			Name: "tmp",
		},
	}
	resp := CreateCubebox(t, req)
	assert.Equal(t, errorcode.ErrorCode_InvalidParamFormat, resp.GetRet().GetRetCode())
	if resp.SandboxID != "" {
		DestroyCubeboxSuccess(t, resp.SandboxID)
	}
}

func TestCubeboxCreateNoResource(t *testing.T) {
	req := SimpleCubeboxConfig()

	for i := range req.GetContainers() {
		req.Containers[i].Resources = nil
	}
	resp := CreateCubebox(t, req)
	assert.Equal(t, errorcode.ErrorCode_InvalidParamFormat, resp.GetRet().GetRetCode())
	if resp.SandboxID != "" {
		DestroyCubeboxSuccess(t, resp.SandboxID)
	}
}

func TestCubeboxCreateEmptyProbe(t *testing.T) {
	req := SimpleCubeboxConfig()
	req.Containers[0].Probe = &cubebox.Probe{
		ProbeHandler: &cubebox.ProbeHandler{},
	}
	resp := CreateCubebox(t, req)
	assert.Equal(t, errorcode.ErrorCode_InvalidParamFormat, resp.GetRet().GetRetCode())
	if resp.SandboxID != "" {
		DestroyCubeboxSuccess(t, resp.SandboxID)
	}
}

func TestCubeboxCreateProbeInvalidPort(t *testing.T) {
	req := SimpleCubeboxConfig()
	req.Containers[0].Probe = &cubebox.Probe{
		ProbeHandler: &cubebox.ProbeHandler{
			TcpSocket: &cubebox.TCPSocketAction{
				Port: -1,
			},
		},
	}
	resp := CreateCubebox(t, req)
	assert.Equal(t, errorcode.ErrorCode_InvalidParamFormat, resp.GetRet().GetRetCode())
	if resp.SandboxID != "" {
		DestroyCubeboxSuccess(t, resp.SandboxID)
	}
}

func TestCubeboxCreateProbeInvalidTimeout(t *testing.T) {
	req := SimpleCubeboxConfig()
	req.Containers[0].Probe = &cubebox.Probe{
		ProbeHandler: &cubebox.ProbeHandler{
			TcpSocket: &cubebox.TCPSocketAction{
				Port: 8080,
			},
		},
		TimeoutMs: 0,
	}
	resp := CreateCubebox(t, req)
	assert.Equal(t, errorcode.ErrorCode_InvalidParamFormat, resp.GetRet().GetRetCode())
	if resp.SandboxID != "" {
		DestroyCubeboxSuccess(t, resp.SandboxID)
	}
}

func TestCubeboxCreateImageNotFound(t *testing.T) {
	req := SimpleCubeboxConfig()
	req.Containers[0].Image = &images.ImageSpec{
		Image: "notfound",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := cubeClient.Create(ctx, req)
	assert.NoError(t, err)
	assert.Equal(t, errorcode.ErrorCode_PullImageFailed, resp.GetRet().GetRetCode())
	if resp.SandboxID != "" {
		DestroyCubeboxSuccess(t, resp.SandboxID)
	}
}

func TestCubeboxCreateNoCwd(t *testing.T) {
	req := SimpleCubeboxConfig()
	req.Containers[0].WorkingDir = ""

	resp := CreateCubebox(t, req)
	assert.Equal(t, errorcode.ErrorCode_Success, resp.GetRet().GetRetCode())
	if resp.SandboxID != "" {
		DestroyCubeboxSuccess(t, resp.SandboxID)
	}
}

func TestCubeboxWritableRootfs(t *testing.T) {
	cfg := SimpleCubeboxConfig()
	cfg.Containers[0].Command = []string{"sh", "-e", "-c", "touch /test; top"}
	cfg.Containers[0].VolumeMounts = []*cubebox.VolumeMounts{
		{
			Name:          "tmp",
			ContainerPath: "/",
		},
		{
			Name:          "tmpfs",
			ContainerPath: "/mytmpfs",
		},
	}
	cfg.Containers[0].SecurityContext.ReadonlyRootfs = false
	id := CreateCubeboxSuccess(t, cfg)

	time.Sleep(10 * time.Millisecond)
	list := ListCubebox(t, &cubebox.ListCubeSandboxRequest{
		Id: &id,
	})
	require.Len(t, list, 1)

	assert.Equal(t, cubebox.ContainerState_CONTAINER_RUNNING, list[0].Containers[0].State)

	if id != "" {
		DestroyCubeboxSuccess(t, id)
	}
}

func TestCubeboxWritableRootfsInvalidMount(t *testing.T) {
	cfg := SimpleCubeboxConfig()
	cfg.Containers[0].Command = []string{"sh", "-e", "-c", "touch /test; top"}
	cfg.Containers[0].SecurityContext.ReadonlyRootfs = false

	resp := CreateCubebox(t, cfg)
	assert.Equal(t, errorcode.ErrorCode_InvalidParamFormat, resp.GetRet().GetRetCode())
	if resp.SandboxID != "" {
		DestroyCubeboxSuccess(t, resp.SandboxID)
	}
}

func TestCubeboxWritableWithoutVolume(t *testing.T) {
	cfg := SimpleCubeboxConfig()
	cfg.Containers[0].Command = []string{"sh", "-e", "-c", "touch /test; top"}
	cfg.Containers[0].VolumeMounts = []*cubebox.VolumeMounts{
		{
			Name:          "tmp",
			ContainerPath: "/",
		},
		{
			Name:          "tmpfs",
			ContainerPath: "/mytmpfs",
		},
	}
	cfg.Volumes = []*cubebox.Volume{
		{
			Name: "tmpfs",
			VolumeSource: &cubebox.VolumeSource{
				EmptyDir: &cubebox.EmptyDirVolumeSource{
					Medium:    1,
					SizeLimit: "16Mi",
				},
			},
		},
	}
	cfg.Containers[0].SecurityContext.ReadonlyRootfs = false

	resp := CreateCubebox(t, cfg)
	assert.Equal(t, errorcode.ErrorCode_InvalidParamFormat, resp.GetRet().GetRetCode())
	if resp.SandboxID != "" {
		DestroyCubeboxSuccess(t, resp.SandboxID)
	}
}

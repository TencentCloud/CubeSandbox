// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package integration

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func testGetCreateCubeSandboxReq() *types.CreateCubeSandboxReq {
	return &types.CreateCubeSandboxReq{
		Request: &types.Request{
			RequestID: uuid.New().String(),
		},
		Timeout:      types.TimeoutPtr(30),
		InstanceType: "cubebox",
		Volumes: []*types.Volume{
			{
				Name: "workdir",
				VolumeSource: &types.VolumeSource{
					EmptyDir: &types.EmptyDirVolumeSource{SizeLimit: "512Mi"},
				},
			},
			{
				Name: "tmp",
				VolumeSource: &types.VolumeSource{
					EmptyDir: &types.EmptyDirVolumeSource{SizeLimit: "512Mi"},
				},
			},
		},
		Containers: []*types.Container{
			{
				Name: "cubebox-runtime-sidecar",
				Image: &types.ImageSpec{
					Image: "busybox:latest",
				},
				Command: []string{"sleep", "30"},
				Resources: &types.Resource{
					Cpu: "100m",
					Mem: "64Mi",
				},
				VolumeMounts: []*cubeboxv1.VolumeMounts{
					{Name: "tmp", ContainerPath: "/tmp"},
				},
			},
			{
				Name: "cubebox-runtime-frame",
				Image: &types.ImageSpec{
					Image: "busybox:latest",
				},
				Command: []string{"sleep", "30"},
				Resources: &types.Resource{
					Cpu: "200m",
					Mem: "256Mi",
				},
				VolumeMounts: []*cubeboxv1.VolumeMounts{
					{Name: "workdir", ContainerPath: "/workspace"},
					{Name: "tmp", ContainerPath: "/tmp"},
				},
			},
		},
		Annotations: map[string]string{
			"com.cube.debug":   "true",
			"com.invoke_port":  "8080",
			"com.netid":        "gw-axgkcimt",
			constants.Caller:   constants.Caller,
			"cube.master.vips": "x.x.x.x;x.x.x.x",
		},
	}
}

func testCreateSandboxByReq(reqC *types.CreateCubeSandboxReq) (rsp *types.CreateCubeSandboxRes) {
	rsp = &types.CreateCubeSandboxRes{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	// Idle TTL is not a valid HTTP deadline; use a fixed test timeout instead.
	timeoutSec := 30
	if reqC.Timeout != nil && *reqC.Timeout > 0 {
		timeoutSec = *reqC.Timeout + 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	reqC.RequestID = uuid.New().String()
	url := getBaseURL("/cube/sandbox")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer([]byte(utils.InterfaceToString(reqC))))
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
		return
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
		return
	}
	if http.StatusOK != resp.StatusCode {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		return
	}
	err = getBodyData(resp, rsp)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
	}
	return
}

func testCreateSandbox(insID string) (rsp *types.CreateCubeSandboxRes) {
	reqC := testGetCreateCubeSandboxReq()
	reqC.InsId = insID
	return testCreateSandboxByReq(reqC)
}

func testDestroySandbox(sandboxID string) (rsp *types.DeleteCubeSandboxRes) {
	rsp = &types.DeleteCubeSandboxRes{
		SandboxID: sandboxID,
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	reqC := &types.DeleteCubeSandboxReq{
		RequestID:    uuid.New().String(),
		SandboxID:    sandboxID,
		InstanceType: "cubebox",
	}
	body, err := types.FastestJsoniter.Marshal(reqC)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
		return
	}
	url := getBaseURL("/cube/sandbox")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, bytes.NewBuffer(body))
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
		return
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
		return
	}
	if http.StatusOK != resp.StatusCode {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		return
	}

	err = getBodyData(resp, rsp)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
	}
	return
}

func testGetSandboxInfoBySandboxID(sandboxID string) (rsp *types.GetCubeSandboxRes) {
	rsp = &types.GetCubeSandboxRes{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	url := getBaseURL("/cube/sandbox/info?requestID=%s&sandbox_id=%s")
	url = fmt.Sprintf(url, uuid.New().String(), sandboxID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
		return
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
		return
	}
	if http.StatusOK != resp.StatusCode {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		return
	}

	err = getBodyData(resp, rsp)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
	}
	return
}

func testGetSandboxInfoByInsID(insID string) (rsp *types.GetCubeSandboxRes) {
	rsp = &types.GetCubeSandboxRes{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	url := getBaseURL("/cube/sandbox/info?requestID=%s&host_id=%s")
	url = fmt.Sprintf(url, uuid.New().String(), insID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
		return
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
		return
	}
	if http.StatusOK != resp.StatusCode {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		return
	}

	err = getBodyData(resp, rsp)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
	}
	return
}

func testListSandboxByInsID(insID string) (rsp *types.ListCubeSandboxRes) {
	rsp = &types.ListCubeSandboxRes{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	url := getBaseURL("/cube/sandbox/list?requestID=%s&host_id=%s")
	url = fmt.Sprintf(url, uuid.New().String(), insID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
		return
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
		return
	}
	if http.StatusOK != resp.StatusCode {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		return
	}

	err = getBodyData(resp, rsp)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
	}
	return
}

func testListSandboxByPages(index, size int) (rsp *types.ListCubeSandboxRes) {
	rsp = &types.ListCubeSandboxRes{
		Ret: &types.Ret{
			RetCode: int(errorcode.ErrorCode_Success),
			RetMsg:  errorcode.ErrorCode_Success.String(),
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	url := getBaseURL("/cube/sandbox/list?requestID=%s&start_idx=%d&size=%d&filter.label_selector=%s=%s")
	url = fmt.Sprintf(url, uuid.New().String(), index, size, constants.Caller, constants.Caller)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
		return
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
		return
	}
	if http.StatusOK != resp.StatusCode {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		return
	}

	err = getBodyData(resp, rsp)
	if err != nil {
		rsp.Ret.RetCode = int(errorcode.ErrorCode_ReqCubeAPIFailed)
		rsp.Ret.RetMsg = err.Error()
	}
	return
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package integration

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeleterrorcode "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

func TestBackoff(t *testing.T) {

}

func TestCreateImage(t *testing.T) {
	mocktest_InitGlobalResources()
	registerCleanup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	url := getBaseURL("/cube/image")
	reqV := &types.CreateImageReq{
		RequestID: uuid.New().String(),
		Image:     "busybox:latest",
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer([]byte(utils.InterfaceToString(reqV))))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	retNode := &types.Res{}
	assert.Nil(t, getBodyData(resp, retNode))
	assert.Equal(t, 200, retNode.Ret.RetCode)
	time.Sleep(2 * time.Second)

	nodes := localcache.GetHealthyNodes(-1)
	for _, node := range nodes {
		calleeEp := cubelet.GetCubeletAddr(node.HostIP())
		mocktest_cubeimagelock.RLock()
		m, ok := mocktest_cubeimageMap[calleeEp]
		if ok {
			got, ok := m[reqV.Image]
			assert.Equal(t, true, ok)
			assert.Equal(t, reqV.Image, got.Image)
		}
		mocktest_cubeimagelock.RUnlock()
	}
	sleep_before_check := 5 * time.Second
	testDeleteImage(t)
	time.Sleep(sleep_before_check)
	for _, node := range nodes {
		calleeEp := cubelet.GetCubeletAddr(node.HostIP())
		mocktest_cubeimagelock.RLock()
		m, ok := mocktest_cubeimageMap[calleeEp]
		if ok {
			_, ok := m[reqV.Image]
			assert.Equal(t, false, ok)
		}
		mocktest_cubeimagelock.RUnlock()
	}
}
func TestCreateImageLimitRetry(t *testing.T) {
	mocktest_InitGlobalResources()
	registerCleanup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	defer func() {
		mock_delCubletErrCode("DeleteImage")
		time.Sleep(5 * time.Second)
		mocktest_cubeimagelock.Lock()
		defer mocktest_cubeimagelock.Unlock()
		mocktest_cubeimageMap = make(map[string]map[string]*images.ImageSpec)
	}()
	url := getBaseURL("/cube/image")
	reqV := &types.CreateImageReq{
		RequestID: uuid.New().String(),
		Image:     "busybox:latest",
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer([]byte(utils.InterfaceToString(reqV))))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	retNode := &types.Res{}
	assert.Nil(t, getBodyData(resp, retNode))
	assert.Equal(t, 200, retNode.Ret.RetCode)
	time.Sleep(2 * time.Second)

	nodes := localcache.GetHealthyNodes(-1)
	for _, node := range nodes {
		calleeEp := cubelet.GetCubeletAddr(node.HostIP())
		mocktest_cubeimagelock.RLock()
		m, ok := mocktest_cubeimageMap[calleeEp]
		if ok {
			got, ok := m[reqV.Image]
			assert.Equal(t, true, ok)
			assert.Equal(t, reqV.Image, got.Image)
		}
		mocktest_cubeimagelock.RUnlock()
	}

	sleep_before_check := 10 * time.Second
	mock_setCubletErrorCode("DeleteImage", errorcode.MasterCode(cubeleterrorcode.ErrorCode_HostDiskNotEnough))
	testDeleteImage(t)
	time.Sleep(sleep_before_check)
	for _, node := range nodes {
		calleeEp := cubelet.GetCubeletAddr(node.HostIP())
		mocktest_cubeimagelock.RLock()
		m, ok := mocktest_cubeimageMap[calleeEp]
		if ok {
			_, ok := m[reqV.Image]

			assert.Equal(t, true, ok)
		}
		mocktest_cubeimagelock.RUnlock()
	}
}

func TestCreateImageLoopRetry(t *testing.T) {
	mocktest_InitGlobalResources()
	registerCleanup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	defer func() {
		mocktest_cubeimagelock.Lock()
		defer mocktest_cubeimagelock.Unlock()
		mocktest_cubeimageMap = make(map[string]map[string]*images.ImageSpec)
	}()

	url := getBaseURL("/cube/image")
	reqV := &types.CreateImageReq{
		RequestID: uuid.New().String(),
		Image:     "busybox:latest",
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer([]byte(utils.InterfaceToString(reqV))))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	retNode := &types.Res{}
	assert.Nil(t, getBodyData(resp, retNode))
	assert.Equal(t, 200, retNode.Ret.RetCode)
	time.Sleep(2 * time.Second)

	nodes := localcache.GetHealthyNodes(-1)
	for _, node := range nodes {
		calleeEp := cubelet.GetCubeletAddr(node.HostIP())
		mocktest_cubeimagelock.RLock()
		m, ok := mocktest_cubeimageMap[calleeEp]
		if ok {
			got, ok := m[reqV.Image]
			assert.Equal(t, true, ok)
			assert.Equal(t, reqV.Image, got.Image)
		}
		mocktest_cubeimagelock.RUnlock()
	}

	sleep_before_del := 10 * time.Second
	sleep_before_check := 20 * time.Second

	mock_setCubletErrorCode("DeleteImage", errorcode.ErrorCode_MasterRateLimitedError)
	go func() {
		time.Sleep(sleep_before_del)
		mock_delCubletErrCode("DeleteImage")
	}()
	testDeleteImage(t)

	time.Sleep(sleep_before_check)
	for _, node := range nodes {
		calleeEp := cubelet.GetCubeletAddr(node.HostIP())
		mocktest_cubeimagelock.RLock()
		m, ok := mocktest_cubeimageMap[calleeEp]
		if ok {
			_, ok := m[reqV.Image]
			assert.Equal(t, false, ok)
		}
		mocktest_cubeimagelock.RUnlock()
	}
}

func testDeleteImage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	url := getBaseURL("/cube/image")
	reqV := &types.CreateImageReq{
		RequestID: uuid.New().String(),
		Image:     "busybox:latest",
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, bytes.NewBuffer([]byte(utils.InterfaceToString(reqV))))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	retNode := &types.Res{}
	assert.Nil(t, getBodyData(resp, retNode))
	assert.Equal(t, 200, retNode.Ret.RetCode)
}

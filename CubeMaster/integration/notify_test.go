// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package integration

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func testNotify(t *testing.T, hostIds []string, which localcache.EventType) {
	reqV := &types.HostChangeEvent{
		Request: &types.Request{
			RequestID: uuid.New().String(),
		},
		HostIDs:   hostIds,
		EventType: string(which),
	}
	doReqWithCommonRes(t, getBaseURL("/notify/host"), http.MethodPost, reqV)
}

func TestHostChangeNotify(t *testing.T) {
	mocktest_InitGlobalResources()
	registerCleanup(t)
	expectedTotalNodes := mocktest_totalnode
	assert.Equal(t, expectedTotalNodes, localcache.GetHealthyNodes(-1).Len())

	hostInfo := newHostInfo(1)

	mocktest_AddHost(hostInfo)
	defer func() {
		mocktest_delHost(hostInfo.InsID)
		time.Sleep(2 * time.Second)
		testNotify(t, []string{hostInfo.InsID}, localcache.DEL)
		time.Sleep(3 * time.Second)
	}()

	testNotify(t, []string{hostInfo.InsID}, localcache.ADD)
	time.Sleep(3 * time.Second)

	retNode, err := testInternalGetNode(hostInfo.InsID)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, 200, retNode.Ret.RetCode)
	if 1 != len(retNode.Data) {
		t.Fatal("node count not match")
	}
	if len(retNode.Data) > 0 {
		assert.Equal(t, hostInfo.InsID, retNode.Data[0].InsID)
		assert.Equal(t, true, retNode.Data[0].Healthy)
	}
	assert.Equal(t, expectedTotalNodes+1, localcache.GetHealthyNodes(-1).Len())
	assert.Equal(t, expectedTotalNodes+1, localcache.GetNodes(-1).Len())

	hostInfo.LiveStatus = "not_live"
	mocktest_updateHost(hostInfo)
	testNotify(t, []string{hostInfo.InsID}, localcache.UPDATE)
	time.Sleep(3 * time.Second)

	retNode, err = testInternalGetNode(hostInfo.InsID)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, 200, retNode.Ret.RetCode)
	assert.Equal(t, 1, len(retNode.Data))
	assert.Equal(t, hostInfo.InsID, retNode.Data[0].InsID)
	assert.Equal(t, false, retNode.Data[0].Healthy)
	assert.Equal(t, expectedTotalNodes, localcache.GetHealthyNodes(-1).Len())
	assert.Equal(t, expectedTotalNodes+1, localcache.GetNodes(-1).Len())

	mocktest_delHost(hostInfo.InsID)
	testNotify(t, []string{hostInfo.InsID}, localcache.DEL)
	time.Sleep(3 * time.Second)

	retNode, err = testInternalGetNode(hostInfo.InsID)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, int(errorcode.ErrorCode_NotFound), retNode.Ret.RetCode)
	assert.Equal(t, expectedTotalNodes, localcache.GetHealthyNodes(-1).Len())
	assert.Equal(t, expectedTotalNodes, localcache.GetNodes(-1).Len())
	sorted := localcache.GetNodes(-1).AllSortByIndex()
	assert.Equal(t, int(1), sorted[0].Index)
}

func TestHealthCheck(t *testing.T) {
	mocktest_InitGlobalResources()
	registerCleanup(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	url := getBaseURL("/notify/health")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	ret := &types.Res{}
	assert.Nil(t, getBodyData(resp, ret))
	assert.Equal(t, 200, ret.Ret.RetCode)
}

func testInternalGetNode(insID string) (*types.GetNodeRes, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	url := getBaseURL("/internal/node?requestID=%s&host_id=%s")
	url = fmt.Sprintf(url, uuid.New().String(), insID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	retNode := &types.GetNodeRes{}
	err = getBodyData(resp, retNode)
	return retNode, err
}

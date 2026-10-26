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
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestGetNodeInfo(t *testing.T) {
	mocktest_InitGlobalResources()
	registerCleanup(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	url := getBaseURL("/internal/node?requestID=%s")
	url = fmt.Sprintf(url, uuid.New().String())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	retNode := &types.GetNodeRes{}
	assert.Nil(t, getBodyData(resp, retNode))
	assert.Equal(t, 200, retNode.Ret.RetCode)
	assert.Equal(t, len(mocktest_allnodeIds), len(retNode.Data))
}

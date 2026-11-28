// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

func TestUpdateNodeStatus_CanonicalInventory(t *testing.T) {
	svc, fs := newTestService(t)
	ctx := context.Background()
	if _, err := svc.RegisterNode(ctx, &model.RegisterNodeRequest{NodeID: "node-1"}); err != nil {
		t.Fatal(err)
	}
	a := model.LocalTemplate{TemplateID: "a", ID: "one", Namespace: "default"}
	b := model.LocalTemplate{TemplateID: "b", ID: "two", Namespace: "default"}
	request := &model.UpdateNodeStatusRequest{
		HeartbeatTime: time.Now(), Conditions: []model.NodeCondition{{Type: "Ready", Status: "True"}},
		LocalTemplates: []model.LocalTemplate{b, a},
		Images:         []model.ContainerImage{{Names: []string{"z", "a"}}},
	}
	if _, err := svc.UpdateNodeStatus(ctx, "node-1", request); err != nil {
		t.Fatal(err)
	}
	first, err := fs.GetStatus(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	request.LocalTemplates = []model.LocalTemplate{a, b}
	request.Images = []model.ContainerImage{{Names: []string{"a", "z"}}}
	request.HeartbeatTime = request.HeartbeatTime.Add(time.Second)
	snapshot, err := svc.UpdateNodeStatus(ctx, "node-1", request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fs.GetStatus(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.LocalTemplatesJSON != second.LocalTemplatesJSON || first.ImagesJSON != second.ImagesJSON {
		t.Fatal("old clients' order-only inventory changes reached persistence")
	}
	if second.HeartbeatUnix != request.HeartbeatTime.Unix() || snapshot.LocalTemplates[0].TemplateID != "a" {
		t.Fatal("normalization lost the fresh heartbeat or inventory")
	}
}

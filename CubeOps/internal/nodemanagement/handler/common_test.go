// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/handler"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/service"
)

// A node without labels must still expose "labels": {} — clients distinguish
// "no labels" from "this server does not support labels" by field presence.
func TestToNodeView_EmptyLabelsIsEmptyObject(t *testing.T) {
	view := handler.ToNodeView(&model.NodeSnapshot{NodeID: "n-1"}, nil)
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	labels, ok := decoded["labels"]
	if !ok {
		t.Fatal("NodeView must always carry a labels field")
	}
	if string(labels) != "{}" {
		t.Fatalf("labels = %s, want {}", labels)
	}
}

func TestToNodeView_LabelsAreACopy(t *testing.T) {
	src := map[string]string{model.LabelMachineType: "intel-k66"}
	view := handler.ToNodeView(&model.NodeSnapshot{NodeID: "n-1", Labels: src}, nil)
	view.Labels["zone"] = "gz"
	if _, ok := src["zone"]; ok {
		t.Fatal("NodeView must not alias the snapshot's label map")
	}
}

func TestMapNodeError_LabelErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"invalid value", service.ErrInvalidLabel, http.StatusBadRequest},
		{"reserved key", service.ErrReservedLabel, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPatch, "/labels", nil)
			handler.MapNodeError(c, tc.err)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestMapNodeError_WrappedLabelErrors(t *testing.T) {
	// Service errors are wrapped with %w; the mapping must survive wrapping.
	if err := service.ValidateMachineTypeValue("AMD-K66"); !errors.Is(err, service.ErrInvalidLabel) {
		t.Fatalf("ValidateMachineTypeValue error = %v, want ErrInvalidLabel", err)
	}
	if err := service.ValidateLabelKey(model.LabelSchedulingDisabled); !errors.Is(err, service.ErrReservedLabel) {
		t.Fatalf("ValidateLabelKey error = %v, want ErrReservedLabel", err)
	}
}

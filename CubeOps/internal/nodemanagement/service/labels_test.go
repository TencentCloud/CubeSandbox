// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/service"
)

func TestValidateLabelKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"app", true},
		{"example.com/app", true},
		{"", false},
		{"a/b/c", false},
		{"cube.cloud.tencentcloud.com/scheduling-disabled", false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			err := service.ValidateLabelKey(tc.key)
			got := err == nil
			if got != tc.want {
				t.Errorf("ValidateLabelKey(%q) = %v, want ok=%v", tc.key, err, tc.want)
			}
		})
	}
}

func TestStripAndPreserveSchedulingLabel(t *testing.T) {
	existing := map[string]string{"zone": "gz", model.LabelSchedulingDisabled: model.LabelSchedulingDisabledValue}
	cubelet := map[string]string{"rack": "r1", model.LabelSchedulingDisabled: "false"}
	got := service.StripAndPreserveSchedulingLabel(existing, cubelet)
	if got[model.LabelSchedulingDisabled] != model.LabelSchedulingDisabledValue {
		t.Errorf("scheduling label overwritten: %s", got[model.LabelSchedulingDisabled])
	}
	if got["rack"] != "r1" {
		t.Errorf("rack missing")
	}
}

func TestDecodeSchedulingDisabled(t *testing.T) {
	if !service.DecodeSchedulingDisabled(map[string]string{model.LabelSchedulingDisabled: "true"}) {
		t.Error("expected disabled")
	}
	if service.DecodeSchedulingDisabled(map[string]string{"zone": "gz"}) {
		t.Error("expected enabled")
	}
}

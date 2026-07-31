// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
)

func TestEnvdPortFromLabels(t *testing.T) {
	tests := []struct {
		name    string
		labels  map[string]string
		primary bool
		want    int32
	}{
		{name: "metadata", labels: map[string]string{constants.LabelEnvdPort: "55000"}, want: 55000},
		{name: "legacy primary fallback", primary: true, want: 49983},
		{name: "legacy sidecar unavailable", primary: false, want: 0},
		{name: "invalid metadata", labels: map[string]string{constants.LabelEnvdPort: "70000"}, primary: true, want: 49983},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := envdPortFromLabels(test.labels, test.primary); got != test.want {
				t.Fatalf("envdPortFromLabels() = %d, want %d", got, test.want)
			}
		})
	}
}

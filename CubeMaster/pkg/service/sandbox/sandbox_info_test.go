// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
)

func TestEnvdPortFromLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   int32
	}{
		{
			name:   "nil labels",
			labels: nil,
			want:   0,
		},
		{
			name:   "label missing",
			labels: map[string]string{"foo": "bar"},
			want:   0,
		},
		{
			name: "valid port",
			labels: map[string]string{
				constants.LabelContainerEnvdPort: "49984",
			},
			want: 49984,
		},
		{
			name: "unparseable port stays zero",
			labels: map[string]string{
				constants.LabelContainerEnvdPort: "not-a-port",
			},
			want: 0,
		},
		{
			name: "empty port stays zero",
			labels: map[string]string{
				constants.LabelContainerEnvdPort: "",
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, envdPortFromLabels(tt.labels))
		})
	}
}

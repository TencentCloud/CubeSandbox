// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

func TestMemoryMergeableDecision(t *testing.T) {
	tests := []struct {
		name        string
		hostAllowed bool
		annotations map[string]string
		requested   bool
		effective   bool
	}{
		{
			name:        "absent template key",
			hostAllowed: true,
			annotations: nil,
			requested:   false,
			effective:   false,
		},
		{
			name:        "template true and host on",
			hostAllowed: true,
			annotations: map[string]string{constants.MasterAnnotationMemoryMergeable: "true"},
			requested:   true,
			effective:   true,
		},
		{
			name:        "template true and host off",
			hostAllowed: false,
			annotations: map[string]string{constants.MasterAnnotationMemoryMergeable: "true"},
			requested:   true,
			effective:   false,
		},
		{
			name:        "template false and host on",
			hostAllowed: true,
			annotations: map[string]string{constants.MasterAnnotationMemoryMergeable: "false"},
			requested:   false,
			effective:   false,
		},
		{
			name:        "case insensitive true",
			hostAllowed: true,
			annotations: map[string]string{constants.MasterAnnotationMemoryMergeable: " TRUE "},
			requested:   true,
			effective:   true,
		},
		{
			name:        "non canonical value is false",
			hostAllowed: true,
			annotations: map[string]string{constants.MasterAnnotationMemoryMergeable: "1"},
			requested:   false,
			effective:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requested, effective := memoryMergeableDecision(tt.hostAllowed, tt.annotations)
			assert.Equal(t, tt.requested, requested)
			assert.Equal(t, tt.effective, effective)
		})
	}
}

func TestMemoryMergeablePodAnnotations(t *testing.T) {
	got := memoryMergeablePodAnnotations(true, false)
	assert.Equal(t, "false", got[constants.AnnotationMemoryMergeable])
	assert.Equal(t, "true", got[constants.MasterAnnotationMemoryMergeable])

	got = memoryMergeablePodAnnotations(false, true)
	assert.Equal(t, "true", got[constants.AnnotationMemoryMergeable])
	assert.Equal(t, "false", got[constants.MasterAnnotationMemoryMergeable])
}

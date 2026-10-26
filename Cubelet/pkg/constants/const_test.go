// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package constants

import (
	"testing"
)

func TestMakeContainerIDEnvKey(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple name",
			input:    "my-container",
			expected: "CUBE_CONTAINER_ID_MY_CONTAINER",
		},
		{
			name:     "name with special characters",
			input:    "my-container-123",
			expected: "CUBE_CONTAINER_ID_MY_CONTAINER_123",
		},
		{
			name:     "name with uppercase letters",
			input:    "My-Container",
			expected: "CUBE_CONTAINER_ID_MY_CONTAINER",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MakeContainerIDEnvKey(tt.input)
			if result != tt.expected {
				t.Errorf("MakeContainerIDEnvKey(%q) returned %q, expected %q", tt.input, result, tt.expected)
			}
		})
	}
}

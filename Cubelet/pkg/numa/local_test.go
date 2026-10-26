// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package numa

import (
	"testing"
)

func TestNumaNodeCPUsToCores(t *testing.T) {
	tests := []struct {
		name     string
		cpulist  string
		expected map[int]bool
	}{
		{
			name:    "single cpu",
			cpulist: "5",
			expected: map[int]bool{
				5: true,
			},
		},
		{
			name:    "range of cpus",
			cpulist: "1-4",
			expected: map[int]bool{
				1: true,
				2: true,
				3: true,
				4: true,
			},
		},
		{
			name:    "mixed single and range",
			cpulist: "0,2-4,6",
			expected: map[int]bool{
				0: true,
				2: true,
				3: true,
				4: true,
				6: true,
			},
		},
		{
			name:     "empty cpulist",
			cpulist:  "",
			expected: map[int]bool{},
		},
		{
			name:     "invalid cpulist",
			cpulist:  "a,b-c",
			expected: map[int]bool{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := numaNodeCPUsToCores(tt.cpulist)
			if len(result) != len(tt.expected) {
				t.Errorf("expected %d cores, got %d", len(tt.expected), len(result))
			}
			for k, v := range tt.expected {
				if result[k] != v {
					t.Errorf("expected core %d to be %v, got %v", k, v, result[k])
				}
			}
		})
	}
}

func TestRemoveDisabledCpus(t *testing.T) {
	tests := []struct {
		name         string
		cpulist      string
		disabledCpus []string
		expected     string
	}{
		{
			name:         "No disabled CPUs",
			cpulist:      "0-20,30-40",
			disabledCpus: []string{},
			expected:     "0-20,30-40",
		},
		{
			name:         "Disable single CPU",
			cpulist:      "0-20,30-40",
			disabledCpus: []string{"0"},
			expected:     "1-20,30-40",
		},
		{
			name:         "Disable multiple CPUs",
			cpulist:      "0-20,30-40",
			disabledCpus: []string{"0", "1"},
			expected:     "2-20,30-40",
		},
		{
			name:         "Disable range partially",
			cpulist:      "0-20,30-40",
			disabledCpus: []string{"5", "10"},
			expected:     "0-4,6-9,11-20,30-40",
		},
		{
			name:         "Disable all CPUs",
			cpulist:      "0-20,30-40",
			disabledCpus: []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15", "16", "17", "18", "19", "20", "30", "31", "32", "33", "34", "35", "36", "37", "38", "39", "40"},
			expected:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := removeDisabledCpus(tt.cpulist, tt.disabledCpus)
			if result != tt.expected {
				t.Errorf("removeDisabledCpus() = %v, want %v", result, tt.expected)
			}
		})
	}
}

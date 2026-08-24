// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package translator

import (
	"encoding/json"
	"testing"
)

func TestSandboxStateFromInt(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{0, "unknown"},
		{1, "running"},
		{2, "unknown"},
		{3, "unknown"},
		{4, "pausing"},
		{5, "paused"},
		{99, "unknown"},
	}
	for _, test := range tests {
		if got := SandboxStateFromInt(test.status); got != test.want {
			t.Errorf("SandboxStateFromInt(%d) = %q, want %q", test.status, got, test.want)
		}
	}
}

func TestSandboxStateFromRaw(t *testing.T) {
	tests := []struct {
		raw  json.RawMessage
		want string
	}{
		{json.RawMessage(`1`), "running"},
		{json.RawMessage(`"running"`), "running"},
		{json.RawMessage(`4`), "pausing"},
		{json.RawMessage(`"pausing"`), "pausing"},
		{json.RawMessage(`5`), "paused"},
		{json.RawMessage(`"pause"`), "paused"},
		{json.RawMessage(`0`), "unknown"},
		{json.RawMessage(`2`), "unknown"},
		{json.RawMessage(`3`), "unknown"},
		{json.RawMessage(`"3"`), "unknown"},
		{json.RawMessage(`"UNKNOWN"`), "unknown"},
		{json.RawMessage(`99`), "unknown"},
		{json.RawMessage(`"invalid"`), "unknown"},
		{json.RawMessage(`null`), "unknown"},
		{json.RawMessage(`{`), "unknown"},
		{nil, "unknown"},
	}
	for _, test := range tests {
		if got := SandboxStateFromRaw(test.raw); got != test.want {
			t.Errorf("SandboxStateFromRaw(%s) = %q, want %q", test.raw, got, test.want)
		}
	}
}

func TestTransformSandboxListPreservesUnknownState(t *testing.T) {
	raw := json.RawMessage(`{
		"ret":{"ret_code":0},
		"data":[{"sandbox_id":"sb-unknown","status":3,"annotations":{},"labels":{}}]
	}`)
	items, ok := TransformSandboxList(raw).([]map[string]interface{})
	if !ok || len(items) != 1 {
		t.Fatalf("TransformSandboxList() = %#v, want one sandbox", TransformSandboxList(raw))
	}
	if got := items[0]["state"]; got != "unknown" {
		t.Errorf("state = %v, want unknown", got)
	}
}

// TestParseMemoryMB verifies the K8s-quantity-to-MiB conversion used by the
// sandbox detail path. It must agree with CubeMaster's parseMemoryMiB and
// CubeAPI's parse_mem_mib so list and detail report identical memory for the
// same quantity (the core consistency goal of the resource-units fix).
func TestParseMemoryMB(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{name: "empty", input: "", want: 0},
		{name: "invalid", input: "bad", want: 0},
		{name: "zero", input: "0", want: 0},
		{name: "negative", input: "-5Mi", want: 0},
		{name: "mib", input: "512Mi", want: 512},
		{name: "mib round number", input: "2048Mi", want: 2048},
		{name: "gibibytes", input: "2Gi", want: 2048},
		{name: "fractional gibibytes", input: "1.5Gi", want: 1536},
		{name: "decimal mega rounds up", input: "537M", want: 513},
		{name: "decimal giga", input: "2G", want: 1908},
		{name: "kibibytes", input: "1024Ki", want: 1},
		{name: "plain bytes ceil to one mib", input: "1024", want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseMemoryMB(tt.input); got != tt.want {
				t.Fatalf("ParseMemoryMB(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// TestParseCPUMilli covers the K8s-quantity-to-millicore conversion used by the
// sandbox detail path. Sub-core quantities must preserve fractional precision.
func TestParseCPUMilli(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{name: "empty", input: "", want: 0},
		{name: "invalid", input: "bad", want: 0},
		{name: "negative", input: "-5m", want: 0},
		{name: "sub core", input: "500m", want: 500},
		{name: "millicores", input: "2000m", want: 2000},
		{name: "whole cores", input: "2", want: 2000},
		{name: "fractional cores", input: "0.5", want: 500},
		{name: "quarter core", input: "0.25", want: 250},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseCPUMilli(tt.input); got != tt.want {
				t.Fatalf("ParseCPUMilli(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

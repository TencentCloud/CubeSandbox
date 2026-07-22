// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package translator

import (
	"encoding/json"
	"testing"
)

func TestTransformSandboxDetailIncludesTerminalContainerChoices(t *testing.T) {
	raw := json.RawMessage(`{
		"ret":{"ret_code":0},
		"data":[{
			"sandbox_id":"sb-1",
			"status":1,
			"containers":[
				{"container_id":"main","type":"sandbox","status":1,"image":"ubuntu"},
				{"container_id":"sidecar","type":"sidecar","status":1,"image":"helper"}
			]
		}]
	}`)

	result, ok := TransformSandboxDetail(raw).(map[string]interface{})
	if !ok {
		t.Fatalf("result type = %T, want map", TransformSandboxDetail(raw))
	}
	containers, ok := result["containers"].([]map[string]interface{})
	if !ok {
		t.Fatalf("containers = %#v, want []map", result["containers"])
	}
	if got, want := len(containers), 2; got != want {
		t.Fatalf("container count = %d, want %d", got, want)
	}
	if got, want := containers[1]["containerID"], "sidecar"; got != want {
		t.Errorf("second container ID = %#v, want %#v", got, want)
	}
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"encoding/json"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

func TestCreateFromImageWritableDiskReclaimFlagDefaultsOff(t *testing.T) {
	found := false
	for _, f := range TemplateCreateFromImageCommand.Flags {
		if f.GetName() != "writable-disk-reclaim" {
			continue
		}
		found = true
		if _, ok := f.(cli.BoolFlag); !ok {
			t.Fatalf("flag writable-disk-reclaim must be a boolean flag, got %T", f)
		}
	}
	if !found {
		t.Fatal("create-from-image must expose --writable-disk-reclaim")
	}

	if newCreateFromImageContext(t, nil).Bool("writable-disk-reclaim") {
		t.Fatal("--writable-disk-reclaim must default to false")
	}
	if !newCreateFromImageContext(t, []string{"--writable-disk-reclaim"}).Bool("writable-disk-reclaim") {
		t.Fatal("--writable-disk-reclaim must parse as true when passed")
	}
}

func TestCreateTemplateFromImageReqWritableDiskReclaimWireField(t *testing.T) {
	var enabled types.CreateTemplateFromImageReq
	if err := json.Unmarshal([]byte(`{"writable_disk_reclaim":true}`), &enabled); err != nil {
		t.Fatalf("decode enabled request: %v", err)
	}
	body, err := json.Marshal(enabled)
	if err != nil {
		t.Fatalf("marshal enabled request: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode enabled wire body: %v", err)
	}
	value, ok := decoded["writable_disk_reclaim"]
	if !ok {
		t.Fatalf("template request wire body must carry writable_disk_reclaim: %s", body)
	}
	if enabled, _ := value.(bool); !enabled {
		t.Fatalf("writable_disk_reclaim=%v, want true", value)
	}

	body, err = json.Marshal(&types.CreateTemplateFromImageReq{})
	if err != nil {
		t.Fatalf("marshal default request: %v", err)
	}
	decoded = nil
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode default wire body: %v", err)
	}
	if _, ok := decoded["writable_disk_reclaim"]; ok {
		t.Fatalf("default false must stay omitted on the wire: %s", body)
	}
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"encoding/json"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestToVolumeViewOmitsToken(t *testing.T) {
	wire := &volumeWireItem{
		VolumeID:  "vol-1",
		Name:      "vol-1",
		Driver:    "cos",
		Token:     "secret-token",
		RefCount:  2,
		CreatedAt: 1700000000,
	}
	view := toVolumeView(wire)
	if view.VolumeID != "vol-1" || view.Driver != "cos" || view.RefCount != 2 {
		t.Fatalf("unexpected view: %+v", view)
	}
	// volumeView has no Token field; ensure wire token is not copied into JSON tags we care about.
	if view.Name != "vol-1" {
		t.Fatalf("name=%q", view.Name)
	}

	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal view: %v", err)
	}
	for _, key := range []string{"token", "private_data", "privateData"} {
		if _, ok := m[key]; ok {
			t.Fatalf("CLI volumeView must omit %s; got %s", key, raw)
		}
	}
}

func TestEnsureVolumeSuccessRet(t *testing.T) {
	if err := ensureVolumeSuccessRet(&types.Ret{RetCode: 0, RetMsg: "ok"}); err != nil {
		t.Fatalf("ret_code 0 should succeed: %v", err)
	}
	if err := ensureVolumeSuccessRet(&types.Ret{RetCode: 200, RetMsg: "ok"}); err != nil {
		t.Fatalf("ret_code 200 should succeed: %v", err)
	}
	if err := ensureVolumeSuccessRet(&types.Ret{RetCode: 404, RetMsg: "not found"}); err == nil {
		t.Fatal("expected error for ret_code 404")
	}
	if err := ensureVolumeSuccessRet(nil); err == nil {
		t.Fatal("expected error for nil ret")
	}
}

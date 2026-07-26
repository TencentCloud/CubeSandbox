// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

func TestValidatePluginPrivateData(t *testing.T) {
	t.Parallel()

	if err := validatePluginPrivateData(""); err != nil {
		t.Fatalf("empty private_data should be allowed: %v", err)
	}
	if err := validatePluginPrivateData("volumes/vol-1/"); err != nil {
		t.Fatalf("short private_data should be allowed: %v", err)
	}
	exact := strings.Repeat("a", models.MaxPrivateDataLen)
	if err := validatePluginPrivateData(exact); err != nil {
		t.Fatalf("exact max length should be allowed: %v", err)
	}
	tooLong := strings.Repeat("a", models.MaxPrivateDataLen+1)
	if err := validatePluginPrivateData(tooLong); err == nil {
		t.Fatal("expected error for private_data exceeding max length")
	} else if !strings.Contains(err.Error(), "exceeds max length") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestVolumeItemFromRecordOmitsPrivateData(t *testing.T) {
	t.Parallel()

	rec := &models.VolumeRecord{
		VolumeID:    "vol-1",
		Name:        "vol-1",
		Driver:      "cos",
		Token:       "secret-token",
		PrivateData: "volumes/vol-1/",
		RefCount:    1,
		CreatedAt:   time.Unix(1700000000, 0).UTC(),
	}
	item := volumeItemFromRecord(rec)
	if item.VolumeID != "vol-1" || item.Token != "secret-token" || item.Driver != "cos" {
		t.Fatalf("unexpected item: %+v", item)
	}

	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["private_data"]; ok {
		t.Fatalf("wire VolumeItem must not expose private_data: %s", raw)
	}
	if _, ok := m["privateData"]; ok {
		t.Fatalf("wire VolumeItem must not expose privateData: %s", raw)
	}
}

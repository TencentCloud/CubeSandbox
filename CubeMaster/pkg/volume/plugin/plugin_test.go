// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"strings"
	"testing"
)

func TestValidateConfigs_uniqueNames(t *testing.T) {
	err := ValidateConfigs([]Config{
		{Name: "cos", Type: PluginTypeBinary},
		{Name: "cos-rpc", Type: PluginTypeRPC},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateConfigs_rejectsDuplicateName(t *testing.T) {
	err := ValidateConfigs([]Config{
		{Name: "cos", Type: PluginTypeBinary},
		{Name: "cos", Type: PluginTypeRPC},
	})
	if err == nil {
		t.Fatal("expected duplicate name error")
	}
	if !strings.Contains(err.Error(), "duplicate driver name") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateConfigs_rejectsEmptyName(t *testing.T) {
	err := ValidateConfigs([]Config{{Name: "", Type: PluginTypeBinary}})
	if err == nil {
		t.Fatal("expected empty name error")
	}
}

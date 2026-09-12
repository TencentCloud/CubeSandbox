// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package plugin

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

func TestRegisterBuiltinsIncludesResourceFitScore(t *testing.T) {
	registry := NewRegistry()

	if err := RegisterBuiltins(registry); err != nil {
		t.Fatalf("RegisterBuiltins returned an error: %v", err)
	}

	selector, err := registry.BuildScore(
		context.Background(),
		config.SchedulerProfilePluginConf{
			Name:   "resource_fit_score",
			Type:   TypeGo,
			Weight: 0.8,
			Args: map[string]any{
				"imbalance_penalty": 0.5,
			},
		},
	)
	if err != nil {
		t.Fatalf("BuildScore returned an error: %v", err)
	}

	if selector.ID() != "Score/resource_fit_score" {
		t.Fatalf("unexpected selector ID: %s", selector.ID())
	}

	if selector.Weight() != 0.8 {
		t.Fatalf("unexpected selector weight: %f", selector.Weight())
	}

	if selector.Disable() {
		t.Fatal("profile resource fit score should be enabled")
	}
}

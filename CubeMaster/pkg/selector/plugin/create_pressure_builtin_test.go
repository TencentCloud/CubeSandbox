// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package plugin

import (
	"context"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

func TestRegisterBuiltinsIncludesCreateConcurrencyScore(t *testing.T) {
	registry := NewRegistry()

	if err := RegisterBuiltins(registry); err != nil {
		t.Fatalf("RegisterBuiltins returned an error: %v", err)
	}

	selector, err := registry.BuildScore(
		context.Background(),
		config.SchedulerProfilePluginConf{
			Name:   "create_concurrency_score",
			Type:   TypeGo,
			Weight: 0.6,
		},
	)
	if err != nil {
		t.Fatalf("BuildScore returned an error: %v", err)
	}

	if selector.ID() != "Score/create_concurrency_score" {
		t.Fatalf("unexpected selector ID: %s", selector.ID())
	}

	if selector.Weight() != 0.6 {
		t.Fatalf("unexpected selector weight: %f", selector.Weight())
	}

	if selector.Disable() {
		t.Fatal("profile create concurrency score should be enabled")
	}
}

func TestRegisterBuiltinsIncludesTemplateLocalPressure(t *testing.T) {
	registry := NewRegistry()

	if err := RegisterBuiltins(registry); err != nil {
		t.Fatalf("RegisterBuiltins returned an error: %v", err)
	}

	selector, err := registry.BuildScore(
		context.Background(),
		config.SchedulerProfilePluginConf{
			Name:   "template_local_pressure",
			Type:   TypeGo,
			Weight: 0.2,
		},
	)
	if err != nil {
		t.Fatalf("BuildScore returned an error: %v", err)
	}

	if selector.ID() != "Score/template_local_pressure" {
		t.Fatalf("unexpected selector ID: %s", selector.ID())
	}

	if selector.Weight() != 0.2 {
		t.Fatalf("unexpected selector weight: %f", selector.Weight())
	}

	if selector.Disable() {
		t.Fatal("profile template local pressure score should be enabled")
	}
}

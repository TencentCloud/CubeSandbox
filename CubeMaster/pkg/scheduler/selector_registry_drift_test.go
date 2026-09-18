// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/filter"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/selector/score"
)

// TestSelectorAllowlistsMatchRegistries is the real drift gate for Profile
// allowlists vs live filter/score registries. pkg/base/config cannot import
// those packages without a cycle, so the comparison lives here.
func TestSelectorAllowlistsMatchRegistries(t *testing.T) {
	assert.Equal(t, filter.RegisteredFilterNames(), config.AllowedSchedulerFilterNames())
	assert.Equal(t, score.RegisteredScoreNames(), config.AllowedSchedulerScoreNames())
	// plugin_conf validation switches must cover every registry scorer; otherwise
	// a new registration + allowlist entry compiles while fail-closed checks
	// silently no-op (default: continue / return false).
	assert.Equal(t, score.RegisteredScoreNames(), config.ScorerNamesWithPluginConfMissingCheck())

	// Negative-weight rejection must also cover every registry scorer: plain
	// float64 via ScorerNamesWithFloat64WeightCheck, *float64 via
	// ScorerNamesWithPointerWeightCheck (binpack). A sixth scorer that only
	// updates the plugin_conf-missing probe would otherwise escape Init.
	weightCovered := make(map[string]struct{})
	for name := range config.ScorerNamesWithFloat64WeightCheck() {
		weightCovered[name] = struct{}{}
	}
	for name := range config.ScorerNamesWithPointerWeightCheck() {
		weightCovered[name] = struct{}{}
	}
	assert.Equal(t, score.RegisteredScoreNames(), weightCovered)
}

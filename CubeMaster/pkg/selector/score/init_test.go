// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package score

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
)

func TestScoresRegistryKeysMatchAllowedSchedulerScoreNames(t *testing.T) {
	// Keep in sync with config.allowedSchedulerScoreNames / score registry keys.
	want := map[string]struct{}{
		"real_time_weighted_average":    {},
		"multi_factor_weighted_average": {},
		"affinity_score":                {},
		"image_score":                   {},
		"binpack_score":                 {},
	}
	got := make(map[string]struct{}, len(scores))
	for name := range scores {
		got[name] = struct{}{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scores registry keys = %#v, want %#v (must match allowedSchedulerScoreNames)", got, want)
	}
}

func TestBuiltinProfilesConstructWithoutPluginConfig(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	tests := []struct {
		profile string
		wantID  string
	}{
		{profile: config.RuntimeProfileBalancedSpread, wantID: "real_time_weighted_average"},
		{profile: config.RuntimeProfileTemplateLocalityFirst, wantID: "image_score"},
		{profile: config.RuntimeProfileBinpackUtilization, wantID: "binpack_score"},
	}

	for _, tt := range tests {
		t.Run(tt.profile, func(t *testing.T) {
			initSelectorTestConfig(t, fmt.Sprintf(`common: {}
log: {}
scheduler:
  profile: %s
`, tt.profile))

			var selectors []Selector
			if panicked := didPanic(func() {
				selectors = NewSelector(context.Background())
			}); panicked {
				t.Fatalf("NewSelector() panicked for built-in profile %q", tt.profile)
			}
			if len(selectors) != 1 {
				t.Fatalf("len(selectors) = %d, want 1", len(selectors))
			}
			wantID := constants.SelectorScoreID + "/" + tt.wantID
			if selectors[0].ID() != wantID {
				t.Fatalf("selector ID = %q, want %q", selectors[0].ID(), wantID)
			}
		})
	}
}

func TestPluginScorerConstructsWithoutResourceWeights(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	initSelectorTestConfig(t, `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - binpack_score
    plugin_conf:
      binpack_score:
        weight: 0.25
`)

	selectors := NewSelector(context.Background())
	if len(selectors) != 1 {
		t.Fatalf("len(selectors) = %d, want 1", len(selectors))
	}
	if selectors[0].Weight() != 0.25 {
		t.Fatalf("Weight() = %v, want plugin_conf weight 0.25", selectors[0].Weight())
	}
}

func TestResourceWeightsPluginNameDoesNotOverridePluginWeight(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	initSelectorTestConfig(t, `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - binpack_score
    resource_weights:
      binpack_score: 99
    plugin_conf:
      binpack_score:
        weight: 0.25
`)

	selectors := NewSelector(context.Background())
	if len(selectors) != 1 {
		t.Fatalf("len(selectors) = %d, want 1", len(selectors))
	}
	if selectors[0].Weight() != 0.25 {
		t.Fatalf("Weight() = %v, want plugin_conf weight 0.25", selectors[0].Weight())
	}
}

func TestSelectorConstructionSeparatesFactorAndPluginScorers(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	t.Run("nil_resource_weights_skips_legacy_affinity", func(t *testing.T) {
		// Empty profile + nil resource_weights: legacy affinity stays off;
		// new plugin-only scorers still construct without fake weights.
		initSelectorTestConfig(t, `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - affinity_score
      - binpack_score
    plugin_conf:
      affinity_score:
        weight: 1
      binpack_score:
        weight: 1
`)

		selectors := NewSelector(context.Background())
		got := selectorIDs(selectors)
		want := []string{
			constants.SelectorScoreID + "/binpack_score",
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("selector IDs = %v, want %v", got, want)
		}
	})

	t.Run("empty_resource_weights_map_allows_affinity", func(t *testing.T) {
		// Non-nil empty map was never gated by the old == nil check.
		initSelectorTestConfig(t, `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - affinity_score
      - binpack_score
    resource_weights: {}
    plugin_conf:
      affinity_score:
        weight: 1
      binpack_score:
        weight: 1
`)

		selectors := NewSelector(context.Background())
		got := selectorIDs(selectors)
		want := []string{
			constants.SelectorScoreID + "/affinity_score",
			constants.SelectorScoreID + "/binpack_score",
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("selector IDs = %v, want %v", got, want)
		}
	})
}

func TestLegacyAffinitySkippedWithoutProfileAndNilResourceWeights(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	initSelectorTestConfig(t, `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - affinity_score
    plugin_conf:
      affinity_score:
        weight: 1
`)

	selectors := NewSelector(context.Background())
	if len(selectors) != 0 {
		t.Fatalf("len(selectors) = %d, want 0 for legacy affinity + nil resource_weights", len(selectors))
	}
}

func TestDirectBinpackScoreWithoutResourceWeights(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	initSelectorTestConfig(t, `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - binpack_score
    plugin_conf:
      binpack_score:
        weight: 1
`)

	selectors := NewSelector(context.Background())
	if len(selectors) != 1 || selectors[0].ID() != constants.SelectorScoreID+"/binpack_score" {
		t.Fatalf("selectors = %v, want binpack_score only", selectorIDs(selectors))
	}
}

func TestEmptyProfileMissingFactorPluginConfSkipsWithoutPanic(t *testing.T) {
	// Empty-profile compatibility: listing a factor scorer without plugin_conf
	// must not panic during NewSelector. It warns and skips so placement falls
	// back to remaining scorers / equal-weight random rather than crashing.
	if runIsolatedScoreConfigTest(t) {
		return
	}
	initSelectorTestConfig(t, `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - real_time_weighted_average
      - binpack_score
    resource_weights:
      realtime_create_num: 1
    plugin_conf:
      binpack_score:
        weight: 1
`)

	selectors := NewSelector(context.Background())
	if len(selectors) != 1 || selectors[0].ID() != constants.SelectorScoreID+"/binpack_score" {
		t.Fatalf("selectors = %v, want only binpack_score (realtime skipped)", selectorIDs(selectors))
	}
}

func TestProfileConstructsPluginScorersWithoutResourceWeights(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	initSelectorTestConfig(t, `common: {}
log: {}
scheduler:
  profile: plugin_combo
  profiles:
    plugin_combo:
      score:
        enable_scorers:
          - binpack_score
          - affinity_score
  score:
    plugin_conf:
      binpack_score:
        weight: 1
      affinity_score:
        weight: 1
`)

	selectors := NewSelector(context.Background())
	got := selectorIDs(selectors)
	want := []string{
		constants.SelectorScoreID + "/binpack_score",
		constants.SelectorScoreID + "/affinity_score",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("selector IDs = %v, want %v", got, want)
	}
}

func TestMixedScorersLegacyAffinityGateWithNewPlugins(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	initSelectorTestConfig(t, `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - affinity_score
      - binpack_score
      - image_score
    resource_weights:
      image_id: 1
    plugin_conf:
      affinity_score:
        weight: 1
      binpack_score:
        weight: 1
      image_score:
        weight: 1
        enable_weight_factors: [image_id]
`)

	selectors := NewSelector(context.Background())
	got := selectorIDs(selectors)
	want := []string{
		constants.SelectorScoreID + "/affinity_score",
		constants.SelectorScoreID + "/binpack_score",
		constants.SelectorScoreID + "/image_score",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("selector IDs = %v, want %v", got, want)
	}
}

func TestFactorScorerRequiresWeightForEnabledFactor(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	path := filepath.Join(t.TempDir(), "cubemaster.yaml")
	content := `common: {}
log: {}
scheduler:
  profile: bad_image
  profiles:
    bad_image:
      score:
        enable_scorers:
          - image_score
        resource_weights:
          mvm_num: 1
  score:
    plugin_conf:
      image_score:
        weight: 1
        enable_weight_factors: [image_id]
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("CUBE_MASTER_CONFIG_PATH", path)
	_, err := config.Init()
	if err == nil {
		t.Fatal("config.Init() error = nil, want fail-fast for missing positive factor weight")
	}
	for _, sub := range []string{"bad_image", "image_score", "no positive resource weight"} {
		if !strings.Contains(err.Error(), sub) {
			t.Fatalf("config.Init() error = %v, want substring %q", err, sub)
		}
	}
}

func TestZeroPluginWeightDisablesEveryScorer(t *testing.T) {
	if runIsolatedScoreConfigTest(t) {
		return
	}
	initSelectorTestConfig(t, `common: {}
log: {}
scheduler:
  score:
    enable_scorers:
      - real_time_weighted_average
      - multi_factor_weighted_average
      - image_score
      - affinity_score
      - binpack_score
    resource_weights:
      mvm_num: 1
      image_id: 1
    plugin_conf:
      real_time_weighted_average:
        weight: 0
        enable_weight_factors: [mvm_num]
      multi_factor_weighted_average:
        weight: 0
        enable_weight_factors: [mvm_num]
      image_score:
        weight: 0
        enable_weight_factors: [image_id]
      affinity_score:
        weight: 0
      binpack_score:
        weight: 0
`)

	selectors := NewSelector(context.Background())
	if len(selectors) != 5 {
		t.Fatalf("len(selectors) = %d, want 5", len(selectors))
	}
	for _, selector := range selectors {
		if !selector.Disable() {
			t.Errorf("%s Disable() = false, want true for weight: 0", selector.ID())
		}
	}
}

func initSelectorTestConfig(t *testing.T, yamlBody string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cubemaster.yaml")
	if err := os.WriteFile(path, []byte(yamlBody), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("CUBE_MASTER_CONFIG_PATH", path)
	if _, err := config.Init(); err != nil {
		t.Fatalf("config.Init(): %v", err)
	}
}

func selectorIDs(selectors []Selector) []string {
	got := make([]string, 0, len(selectors))
	for _, selector := range selectors {
		got = append(got, selector.ID())
	}
	return got
}

func didPanic(fn func()) (panicked bool) {
	defer func() {
		panicked = recover() != nil
	}()
	fn()
	return false
}

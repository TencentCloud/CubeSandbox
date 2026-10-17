// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
)

func TestBuildingPhaseForRootfsFormat(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{config.RootfsFormatExt4, JobPhaseBuildingExt4},
		{config.RootfsFormatErofs, JobPhaseBuildingErofs},
		{"", JobPhaseBuildingExt4},           // empty (legacy) -> ext4, never erofs
		{"EROFS", JobPhaseBuildingErofs},     // case-insensitive: normalizedRootfsFormat lowercases, so EROFS -> erofs
		{"  erofs  ", JobPhaseBuildingErofs}, // whitespace trimmed too
		{"unknown", JobPhaseBuildingExt4},    // unknown -> ext4 fallback
	}
	for _, c := range cases {
		if got := buildingPhaseForRootfsFormat(c.in); got != c.want {
			t.Fatalf("buildingPhaseForRootfsFormat(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsBuildingPhase(t *testing.T) {
	cases := []struct {
		phase string
		want  bool
	}{
		{JobPhaseBuildingExt4, true},
		{JobPhaseBuildingErofs, true},
		{JobPhasePulling, false},
		{JobPhaseUnpacking, false},
		{JobPhaseGeneratingJSON, false},
		{JobPhaseDistributing, false},
		{"", false},
		{"BUILDING_XFS", false},
	}
	for _, c := range cases {
		if got := isBuildingPhase(c.phase); got != c.want {
			t.Fatalf("isBuildingPhase(%q) = %v, want %v", c.phase, got, c.want)
		}
	}
}

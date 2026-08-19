// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatetypes

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestApplyVersionMap(t *testing.T) {
	local := &LocalRunTemplate{}
	ApplyVersionMap(local, map[string]string{
		CubeComponentCubeShim:  "1.0",
		CubeComponentCubeImage: "img-1",
		"ignored":              "x",
	})
	assert.Equal(t, "1.0", local.Componts[CubeComponentCubeShim].Component.Version)
	assert.Equal(t, "img-1", local.Componts[CubeComponentCubeImage].Component.Version)
	assert.Equal(t, CubeComponentCubeShim, local.Componts[CubeComponentCubeShim].Component.Name)

	// Does not overwrite existing version.
	ApplyVersionMap(local, map[string]string{CubeComponentCubeShim: "2.0"})
	assert.Equal(t, "1.0", local.Componts[CubeComponentCubeShim].Component.Version)
}

func TestVersionMapFromComponts(t *testing.T) {
	local := &LocalRunTemplate{
		Componts: map[string]LocalComponent{
			CubeComponentCubeAgent: {Component: MachineComponent{Version: "a1"}},
			"other":                {Component: MachineComponent{Version: "x"}},
		},
	}
	got := VersionMapFromComponts(local)
	assert.Equal(t, map[string]string{CubeComponentCubeAgent: "a1"}, got)
}

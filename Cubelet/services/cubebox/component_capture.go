// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet/versioninfo"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

const (
	collectorComponentShim = "containerd-shim-cube-rs"
)

// CaptureForCubeBox records inventory version strings on the CubeBox (no paths).
// Existing keys are kept; missing keys are filled from LocalRunTemplate then live toolbox.
func CaptureForCubeBox(cb *cubeboxstore.CubeBox) {
	captureForCubeBox(cb, templatetypes.DefaultToolboxRoot)
}

func captureForCubeBox(cb *cubeboxstore.CubeBox, toolboxRoot string) {
	if cb == nil {
		return
	}

	out := make(map[string]string, 4)
	for name, ver := range cb.ComponentVersions {
		key := templatetypes.InventoryVersionKey(strings.TrimSpace(ver))
		if key == "" {
			continue
		}
		out[name] = key
	}
	seedVersionsFromTemplate(out, cb.LocalRunTemplate)
	if inventoryVersionsComplete(out) {
		cb.ComponentVersions = out
		return
	}

	for name, ver := range inventoryVersionsFromToolbox(toolboxRoot) {
		if _, exists := out[name]; exists {
			continue
		}
		out[name] = ver
	}
	if len(out) == 0 {
		return
	}
	cb.ComponentVersions = out
}

func inventoryVersionsComplete(versions map[string]string) bool {
	for _, name := range []string{
		templatetypes.CubeComponentCubeShim,
		templatetypes.CubeComponentCubeKernel,
		templatetypes.CubeComponentCubeImage,
		templatetypes.CubeComponentCubeAgent,
	} {
		if strings.TrimSpace(versions[name]) == "" {
			return false
		}
	}
	return true
}

func inventoryVersionsFromLive() map[string]string {
	return inventoryVersionsFromToolbox(templatetypes.DefaultToolboxRoot)
}

func inventoryVersionsFromToolbox(toolboxRoot string) map[string]string {
	out := make(map[string]string, 4)
	for _, item := range versioninfo.NewCollector(toolboxRoot).Collect() {
		name, ok := inventoryNameForCollectorComponent(item.Component)
		if !ok {
			continue
		}
		ver := templatetypes.InventoryVersionKey(item.Version)
		if ver == "" {
			continue
		}
		out[name] = ver
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func seedVersionsFromTemplate(out map[string]string, local *templatetypes.LocalRunTemplate) {
	for name, ver := range versionsFromLocalTemplate(local) {
		if _, exists := out[name]; exists {
			continue
		}
		out[name] = ver
	}
}

func inventoryNameForCollectorComponent(component string) (string, bool) {
	switch strings.TrimSpace(component) {
	case versioninfo.ComponentGuestImage:
		return templatetypes.CubeComponentCubeImage, true
	case versioninfo.ComponentKernel:
		return templatetypes.CubeComponentCubeKernel, true
	case versioninfo.ComponentCubeAgent:
		return templatetypes.CubeComponentCubeAgent, true
	case collectorComponentShim:
		return templatetypes.CubeComponentCubeShim, true
	default:
		return "", false
	}
}

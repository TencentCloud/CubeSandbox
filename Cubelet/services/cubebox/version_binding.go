// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"strings"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

type guestEnvironmentVersions struct {
	GuestImage string
	Agent      string
	Kernel     string
	Shim       string
}

// envdVersionFromCubeBox returns the template envd version propagated by
// CubeMaster when the sandbox was created. CubeBox annotations are persisted
// with the sandbox, so snapshot commits can reuse the value without a guest
// Exec on their latency-sensitive success path.
func envdVersionFromCubeBox(cb *cubeboxstore.CubeBox) string {
	if cb == nil {
		return ""
	}
	return strings.TrimSpace(cb.Annotations[constants.MasterAnnotationComponentEnvdVersion])
}

// collectGuestEnvironmentVersions reads the live toolbox once via the same
// inventory path used for catalog ComponentVersions (normalized keys).
func collectGuestEnvironmentVersions() guestEnvironmentVersions {
	return guestEnvironmentVersionsFromComponentMap(inventoryVersionsFromLive(), guestEnvironmentVersions{})
}

// guestEnvironmentVersionsFromCubeBox prefers versions already captured on the
// CubeBox (same source as snapshot catalog) and only scans the live toolbox when
// fields are still missing. Callers should CaptureForCubeBox first.
func guestEnvironmentVersionsFromCubeBox(cb *cubeboxstore.CubeBox) guestEnvironmentVersions {
	var versions map[string]string
	if cb != nil {
		versions = cb.ComponentVersions
	}
	out := guestEnvironmentVersionsFromComponentMap(versions, guestEnvironmentVersions{})
	if guestEnvironmentVersionsComplete(out) {
		return out
	}
	return guestEnvironmentVersionsFromComponentMap(versions, collectGuestEnvironmentVersions())
}

func guestEnvironmentVersionsFromComponentMap(versions map[string]string, fallback guestEnvironmentVersions) guestEnvironmentVersions {
	out := fallback
	if ver := componentVersionFromMap(versions, templatetypes.CubeComponentCubeImage); ver != "" {
		out.GuestImage = ver
	}
	if ver := componentVersionFromMap(versions, templatetypes.CubeComponentCubeAgent); ver != "" {
		out.Agent = ver
	}
	if ver := componentVersionFromMap(versions, templatetypes.CubeComponentCubeKernel); ver != "" {
		out.Kernel = ver
	}
	if ver := componentVersionFromMap(versions, templatetypes.CubeComponentCubeShim); ver != "" {
		out.Shim = ver
	}
	return out
}

func guestEnvironmentVersionsComplete(v guestEnvironmentVersions) bool {
	return strings.TrimSpace(v.GuestImage) != "" &&
		strings.TrimSpace(v.Agent) != "" &&
		strings.TrimSpace(v.Kernel) != "" &&
		strings.TrimSpace(v.Shim) != ""
}

func componentVersionFromMap(versions map[string]string, name string) string {
	if len(versions) == 0 {
		return ""
	}
	return templatetypes.InventoryVersionKey(strings.TrimSpace(versions[name]))
}

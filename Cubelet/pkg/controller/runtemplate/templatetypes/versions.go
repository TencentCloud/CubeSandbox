// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatetypes

import (
	"strings"
)

// ApplyVersionMap fills empty Componts Version fields from versions.
func ApplyVersionMap(local *LocalRunTemplate, versions map[string]string) {
	if local == nil || len(versions) == 0 {
		return
	}
	if local.Componts == nil {
		local.Componts = make(map[string]LocalComponent)
	}
	for name, ver := range versions {
		name = strings.TrimSpace(name)
		ver = strings.TrimSpace(ver)
		if name == "" || ver == "" || !IsVersionedInventoryComponent(name) {
			continue
		}
		lc := local.Componts[name]
		if strings.TrimSpace(lc.Component.Version) == "" {
			lc.Component.Name = name
			lc.Component.Version = ver
			local.Componts[name] = lc
		}
	}
}

// VersionMapFromComponts returns Name→Version for inventory components.
func VersionMapFromComponts(local *LocalRunTemplate) map[string]string {
	if local == nil || local.Componts == nil {
		return nil
	}
	out := make(map[string]string)
	for name, lc := range local.Componts {
		ver := strings.TrimSpace(lc.Component.Version)
		if ver == "" || !IsVersionedInventoryComponent(name) {
			continue
		}
		out[name] = ver
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

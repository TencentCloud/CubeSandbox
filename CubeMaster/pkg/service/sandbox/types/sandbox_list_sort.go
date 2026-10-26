// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package types

import (
	"sort"

	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// IsPausedBrief reports whether the list row is a paused sandbox (no live
// shim). Pause bindings are listed after running／exited rows.
func IsPausedBrief(item *SandboxBriefData) bool {
	return item != nil && item.Status == int32(cubeboxv1.ContainerState_CONTAINER_PAUSED)
}

// SortSandboxList puts non-paused rows first (newest CreateAt first), then
// paused rows. SandboxID breaks ties so the order is stable.
func SortSandboxList(items []*SandboxBriefData) {
	sort.SliceStable(items, func(i, j int) bool {
		pi, pj := IsPausedBrief(items[i]), IsPausedBrief(items[j])
		if pi != pj {
			return !pi && pj
		}
		if items[i].CreateAt != items[j].CreateAt {
			return items[i].CreateAt > items[j].CreateAt
		}
		return items[i].SandboxID < items[j].SandboxID
	})
}

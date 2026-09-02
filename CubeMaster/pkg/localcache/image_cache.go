// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"context"

	fwk "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/framework"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func LoadImageScore() {
	totalMap := make(map[string]int, 0)
	totalNodesFn := func(label string) int {
		if total, ok := totalMap[label]; ok {
			return total
		}

		total := GetHealthyNodesByInstanceType(-1, label).Len()
		totalMap[label] = total
		return total
	}

	for _, item := range l.imageCache.Items() {
		state := item.Object.(*fwk.ImageStateSummary)

		state.ScaledImageScore = scaledImageScore(state, totalNodesFn(state.OssClusterLabel))
	}
	CubeLog.WithContext(context.Background()).Errorf("LoadImageScore completed:%d", len(l.imageCache.Items()))
}

func (l *local) addImageCache(name string, state *fwk.ImageStateSummary) {
	l.imageCache.SetDefault(name, state)
}

func (l *local) getImageCache(name string) *fwk.ImageStateSummary {
	v, ok := l.imageCache.Get(name)
	if !ok {
		return nil
	}
	return v.(*fwk.ImageStateSummary)
}

func scaledImageScore(imageState *fwk.ImageStateSummary, totalNumNodes int) int64 {
	if imageState == nil {
		return 0
	}
	if totalNumNodes == 0 {
		return 0
	}
	snStat := imageState.Snapshot()

	spread := float64(snStat.NumNodes) / float64(totalNumNodes)
	return int64(float64(snStat.Size) * spread)
}

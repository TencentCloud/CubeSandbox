// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package chics

import (
	"context"
	"fmt"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"k8s.io/apimachinery/pkg/util/sets"
)

func (v *cubeHostClientManagerLocal) ListAllVmImages(ctx context.Context) (map[string]sets.Set[string], error) {
	cs, err := v.store.GetAll(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("failed to get cube host image reverse servers: %w", err)
	}

	imageDigestMap := make(map[string]sets.Set[string])
	for _, vm := range cs {
		var cubeboxID = vm.factory.sandboxID
		if vm.GetVmClient() == nil {
			return nil, fmt.Errorf("cubebox %s host image vmclient is not ready", cubeboxID)
		}
		nsImageDigestSet, ok := imageDigestMap[vm.ns]
		if !ok {
			nsImageDigestSet = sets.New[string]()
			imageDigestMap[vm.ns] = sets.New[string]()
		}

		var (
			box      *cubebox.CubeBox
			vmImages []string
		)
		if v.cri != nil {
			box, err = v.cri.CubeboxStore().Get(ctx, cubeboxID)
			if err != nil {
				log.G(ctx).Errorf("failed to get cubebox %s : %v", cubeboxID, err)
				continue
			}
		}
		if box != nil {
			vmImages = box.GetOrCreatePodConfig().GetHostedImageList()
		} else {
			log.G(ctx).Errorf("failed to list vm images for sandbox %s : %v", cubeboxID, err)
		}

		imageDigestMap[vm.ns] = nsImageDigestSet.Insert(vmImages...)
	}

	log.G(ctx).WithField("images", log.WithJsonValue(imageDigestMap)).Debugf("ListAllVmImages success")
	return imageDigestMap, nil
}

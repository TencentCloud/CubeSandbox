// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package images

import (
	"context"
	"fmt"
	"sync"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/platforms"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func (c *CubeImageService) CheckImages(ctx context.Context) error {

	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return fmt.Errorf("get namespace: %w", err)
	}
	cImages, err := c.client.ListImages(ctx)
	if err != nil {
		return fmt.Errorf("unable to list images: %w", err)
	}
	log := CubeLog.WithFields(CubeLog.Fields{
		"Namespace": ns,
	})
	log.Infof("Found %d images", len(cImages))

	snapshotter := c.config.Snapshotter
	var wg sync.WaitGroup
	for _, i := range cImages {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()

			log := log.WithFields(CubeLog.Fields{
				"image": i.Name(),
			})
			log.Infof("Checking image readiness")

			ok, _, _, _, err := images.Check(ctx, i.ContentStore(), i.Target(), platforms.Default())
			if err != nil {
				log.Errorf("Failed to check image content readiness: %v", err)
				return
			}
			if !ok {
				log.Warnf("The image content readiness is not ok: %v", err)
				return
			}

			unpacked, err := i.IsUnpacked(ctx, snapshotter)
			if err != nil {
				log.Warnf("Failed to check whether image is unpacked for image: %v", err)
				return
			}
			if !unpacked {
				log.Warnf("The image is not unpacked.")

			}

			log.Infof("Image is ready, start to update reference")
			if err := c.UpdateImage(ctx, i.Name()); err != nil {
				log.Warnf("Failed to update reference for image: %v", err)
				return
			}
			log.Infof("Loaded image success")
		}()
	}
	wg.Wait()

	log.Infof("All images are ready")

	afterImageRecoverForHandler(c)
	return nil
}

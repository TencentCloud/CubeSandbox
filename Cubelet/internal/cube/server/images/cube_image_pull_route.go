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
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/transfer/registry"
	"github.com/containerd/errdefs"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/server/images/ext4image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

func (c *CubeImageService) EnsureImage(ctx context.Context, ref, username, password string, config *runtime.PodSandboxConfig) (containerd.Image, error) {

	var (
		stepLog = log.G(ctx).WithFields(CubeLog.Fields{
			"image": ref,
		})
		start = time.Now()
		err   error
	)

	cubeSpec := constants.GetImageSpec(ctx)
	if cubeSpec != nil && cubeSpec.GetStorageMedia() == cubeimages.ImageStorageMediaType_ext4.String() {
		instanceType := resolveExt4InstanceType(ctx, cubeSpec)
		if instanceType == "" {
			return nil, fmt.Errorf("create req is nil")
		}
		err := ensureExt4ArtifactImage(ctx, instanceType, ref)
		if err != nil {
			return nil, fmt.Errorf("ensure pmem file failed: %v", err)
		}
		return nil, nil
	}

	storedImage, err := c.LocalResolve(ctx, ref)
	defer func() {
		workflow.RecordCreateMetricIfGreaterThan(ctx, err, "CubeImageService.EnsureImage", time.Since(start), time.Millisecond)
	}()
	if err != nil && !errdefs.IsNotFound(err) {
		return nil, fmt.Errorf("failed to get image %q: %w", ref, err)
	}
	if err == nil {
		img, err := c.ToContainerdImage(ctx, storedImage)
		if errdefs.IsNotFound(err) {
			stepLog.Errorf("image %q not found in image service, remove it", ref)
			err = c.RemoveImage(ctx, &runtime.ImageSpec{Image: ref})
			if err != nil {
				return nil, fmt.Errorf("failed to remove old image %q: %w", ref, err)
			}
			stepLog.Errorf("remove old image %q success", ref)
		} else if err != nil {
			return nil, fmt.Errorf("failed to run to containerd image %q: %w", ref, err)
		} else {
			return img, nil
		}
	}

	if username != "" || password != "" {
		if username == "" || password == "" {
			stepLog.Error("username and password should be both provided")
			return nil, fmt.Errorf("username and password should be both provided")
		}
	}

	credentials := func(host string) (string, string, error) {
		return username, password, nil
	}

	stepLog.Infof("start pull image")
	_, err = c.PullImage(ctx, ref, credentials, config)
	if err != nil {
		if errors.Is(err, ErrSkip) {
			return nil, nil
		}

		return nil, ret.Errorf(errorcode.ErrorCode_UpdateLocalImageSpecFailed, "failed to pull image %q: %s", ref, err.Error())
	}
	stepLog.Error("pull image success")

	storedImage, err = c.LocalResolve(ctx, ref)
	if err != nil && !errdefs.IsNotFound(err) {
		return nil, fmt.Errorf("failed to get image %q: %w", ref, err)
	}
	if err == nil {
		stepLog.Debug("resolve image to containerd image")
		return c.ToContainerdImage(ctx, storedImage)
	}
	return nil, err
}

var ErrSkip = errors.New("skip error")

func ensureExt4ArtifactImage(ctx context.Context, instanceType, imageRef string) error {
	if err := ext4image.EnsurePmemRootfs(ctx, instanceType, imageRef); err != nil {
		return err
	}
	return ext4image.RefreshArtifactRuntimeFiles(ctx, instanceType, imageRef)
}

func (c *CubeImageService) PullImage(ctx context.Context, name string, credentials func(string) (string, string, error), sandboxConfig *runtime.PodSandboxConfig) (_ string, err error) {

	c.imageRemoveLock.RLock()
	defer c.imageRemoveLock.RUnlock()

	ctx, done, err := c.client.WithLease(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to create lease: %w", err)
	}
	defer done(ctx)

	if c.imageDeleteScheduler != nil {
		records, err := c.imageDeleteScheduler.getRecords(name)
		if err != nil {
			return "", fmt.Errorf("failed to get image delete records: %w", err)
		}
		if len(records) > 0 {
			return "", fmt.Errorf("image is being deleted, please try again later")
		}
	}

	ref := name

	cubeSpec := constants.GetImageSpec(ctx)
	if cubeSpec == nil {
		cubeSpec = &cubeimages.ImageSpec{}
		if sandboxConfig != nil && len(sandboxConfig.Annotations) > 0 {
			imageSpecStr, ok := sandboxConfig.Annotations[constants.LabelContainerCubeImageSpec]
			if ok {
				err = json.Unmarshal([]byte(imageSpecStr), &cubeSpec)
				if err != nil {
					return "", fmt.Errorf("unmarshal cube image spec failed: %v", err)
				}
				ctx = constants.WithImageSpec(ctx, cubeSpec)
			}
		}
	}
	if cubeSpec != nil {
		mediaType := cubeSpec.StorageMedia
		switch mediaType {
		case cubeimages.ImageStorageMediaType_ext4.String():
			instanceType := resolveExt4InstanceType(ctx, cubeSpec)
			if instanceType == "" {
				return "", fmt.Errorf("create req is nil")
			}
			err := ensureExt4ArtifactImage(ctx, instanceType, ref)
			if err != nil {
				return "", fmt.Errorf("ensure pmem file failed: %v", err)
			}
			return name, ErrSkip
		case "nfs":
			return "", fmt.Errorf("nfs images are not supported in the open source build")
		default:
		}
	}

	return c.PullRegistryImage(ctx, ref, &PullImageOption{
		Credentials:   credentials,
		SandboxConfig: sandboxConfig,
	})
}

type staticCredentials struct {
	ref      string
	username string
	secret   string
}

func NewStaticCredentials(username, token, ref string) registry.CredentialHelper {
	return &staticCredentials{
		ref:      ref,
		username: username,
		secret:   token,
	}
}

func (sc *staticCredentials) GetCredentials(ctx context.Context, ref, host string) (registry.Credentials, error) {
	if ref == sc.ref {
		return registry.Credentials{
			Username: sc.username,
			Secret:   sc.secret,
		}, nil
	}
	return registry.Credentials{}, nil
}

func resolveExt4InstanceType(ctx context.Context, cubeSpec *cubeimages.ImageSpec) string {
	if req := workflow.GetCreateContext(ctx); req != nil && req.GetInstanceType() != "" {
		return req.GetInstanceType()
	}
	if cubeSpec != nil && cubeSpec.Annotations != nil {
		return cubeSpec.Annotations[constants.MasterAnnotationInstanceType]
	}
	return ""
}

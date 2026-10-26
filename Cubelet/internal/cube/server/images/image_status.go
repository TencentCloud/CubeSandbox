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
	"fmt"
	"strconv"
	"strings"

	"github.com/containerd/errdefs"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	imagestore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/util"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

func (c *CubeImageService) ImageStatus(ctx context.Context, r *runtime.ImageStatusRequest) (*runtime.ImageStatusResponse, error) {
	image, err := c.LocalResolve(ctx, r.GetImage().GetImage())
	if err != nil {
		if errdefs.IsNotFound(err) {

			return &runtime.ImageStatusResponse{}, nil
		}
		return nil, fmt.Errorf("can not resolve %q locally: %w", r.GetImage().GetImage(), err)
	}

	runtimeImage := toCRIImage(image)
	info, err := c.toCRIImageInfo(ctx, &image, r.GetVerbose())
	if err != nil {
		return nil, fmt.Errorf("failed to generate image info: %w", err)
	}

	return &runtime.ImageStatusResponse{
		Image: runtimeImage,
		Info:  info,
	}, nil
}

func toCRIImage(image imagestore.Image) *runtime.Image {
	repoTags, repoDigests := util.ParseImageReferences(image.References)
	runtimeImage := &runtime.Image{
		Id:          image.ID,
		RepoTags:    repoTags,
		RepoDigests: repoDigests,
		Size_:       uint64(image.Size),
		Pinned:      image.Pinned,
		Spec: &runtime.ImageSpec{
			Annotations: image.Annotation,
			Image:       image.MediaType,
		},
	}
	uid, username := getUserFromImage(image.ImageSpec.Config.User)
	if uid != nil {
		runtimeImage.Uid = &runtime.Int64Value{Value: *uid}
	}
	runtimeImage.Username = username

	return runtimeImage
}

func getUserFromImage(user string) (*int64, string) {

	if user == "" {
		return nil, ""
	}

	user = strings.Split(user, ":")[0]

	uid, err := strconv.ParseInt(user, 10, 64)
	if err != nil {

		return nil, user
	}

	return &uid, ""
}

type verboseImageInfo struct {
	ImageSpec  imagespec.Image   `json:"imageSpec"`
	ChainID    string            `json:"chainID"`
	Snapshots  []string          `json:"snapshots"`
	UidFiles   string            `json:"uidFiles"`
	MediaType  string            `json:"mediaType"`
	Annotation map[string]string `json:"annotation"`
}

func (c *CubeImageService) toCRIImageInfo(ctx context.Context, image *imagestore.Image, verbose bool) (map[string]string, error) {
	if !verbose {
		return nil, nil
	}

	info := make(map[string]string)

	imi := &verboseImageInfo{
		ChainID:    image.ChainID,
		ImageSpec:  image.ImageSpec,
		Snapshots:  image.Snapshots,
		UidFiles:   image.UidFiles,
		MediaType:  image.MediaType,
		Annotation: image.Annotation,
	}

	m, err := json.Marshal(imi)
	if err == nil {
		info["info"] = string(m)
	} else {
		log.G(ctx).WithError(err).Errorf("failed to marshal info %v", imi)
		info["info"] = err.Error()
	}

	return info, nil
}

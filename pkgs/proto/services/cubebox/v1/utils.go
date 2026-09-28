// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	v1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

func (containerReq *ContainerConfig) IsImageStorageMediaType(mediaType v1.ImageStorageMediaType) bool {
	toCheck := containerReq.GetImage().GetStorageMedia()
	if toCheck == "" {
		toCheck = v1.ImageStorageMediaType_docker.String()
	}

	return toCheck == mediaType.String()
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"encoding/json"
	"fmt"

	v1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

// AnnotationHostMountAllowedPrefixes carries CubeMaster's host-mount allowlist
// to Cubelet. It is internal runtime metadata and must not be accepted from
// users.
const AnnotationHostMountAllowedPrefixes = "cube.master.internal.host_mount.allowed_prefixes"

// EncodeHostMountAllowedPrefixes serializes an allowlist for the internal
// CubeMaster-to-Cubelet annotation.
func EncodeHostMountAllowedPrefixes(prefixes []string) string {
	raw, _ := json.Marshal(prefixes)
	return string(raw)
}

// HostMountAllowedPrefixes decodes the internal allowlist annotation. The
// boolean reports whether the annotation was present.
func HostMountAllowedPrefixes(annotations map[string]string) ([]string, bool, error) {
	raw, ok := annotations[AnnotationHostMountAllowedPrefixes]
	if !ok {
		return nil, false, nil
	}
	var prefixes []string
	if err := json.Unmarshal([]byte(raw), &prefixes); err != nil {
		return nil, true, fmt.Errorf("parse %s: %w", AnnotationHostMountAllowedPrefixes, err)
	}
	return prefixes, true, nil
}

func (containerReq *ContainerConfig) IsImageStorageMediaType(mediaType v1.ImageStorageMediaType) bool {
	toCheck := containerReq.GetImage().GetStorageMedia()
	if toCheck == "" {
		toCheck = v1.ImageStorageMediaType_docker.String()
	}

	return toCheck == mediaType.String()
}

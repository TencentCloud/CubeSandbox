// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package mount

import (
	"path/filepath"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func CanonicalizePath(path string) (string, error) {

	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	return filepath.EvalSymlinks(path)
}

func VolumeMountOptions(mount *cubebox.VolumeMounts) []string {
	mOptions := []string{constants.MountOptBindRO}
	switch mount.GetPropagation() {
	case cubebox.MountPropagation_PROPAGATION_PRIVATE:
		mOptions = append(mOptions, constants.MountPropagationRprivate)
	case cubebox.MountPropagation_PROPAGATION_BIDIRECTIONAL:

		mOptions = append(mOptions, constants.MountPropagationRShared)

	case cubebox.MountPropagation_PROPAGATION_HOST_TO_CONTAINER:

		mOptions = append(mOptions, constants.MountPropagationRSlave)
	default:
		mOptions = append(mOptions, constants.MountPropagationRprivate)
	}

	if mount.GetReadonly() {
		mOptions = append(mOptions, constants.MountOptReadOnly)
	} else {
		mOptions = append(mOptions, constants.MountOptReadWrite)
	}
	return mOptions
}

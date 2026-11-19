// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// rejectUnsupportedXFSS3Resources refuses an xfs-s3 sandbox that depends on
// something its pause checkpoint cannot carry to another node: host paths
// and plugin volumes live outside the package, so a restore elsewhere would
// run against missing or different data. Other backends are unaffected.
func rejectUnsupportedXFSS3Resources(req *types.CreateCubeSandboxReq, cubeletReq *cubebox.RunCubeSandboxRequest) error {
	if req == nil {
		return nil
	}
	backend := strings.TrimSpace(req.Backend)
	if cubeletReq != nil {
		if b := strings.TrimSpace(cubeletReq.GetAnnotations()[constants.CubeAnnotationStorageBackend]); b != "" {
			backend = b
		}
	}
	if !constants.IsXFSS3Backend(backend) {
		return nil
	}
	if createRequestHasHostMount(req) {
		return ret.Errorf(errorcode.ErrorCode_MasterParamsError,
			"backend %s does not support host mounts: they cannot be restored on another node", backend)
	}
	for _, vol := range req.Volumes {
		if vol != nil && vol.VolumeSource != nil && vol.VolumeSource.PluginVolume != nil {
			return ret.Errorf(errorcode.ErrorCode_MasterParamsError,
				"backend %s does not support plugin volume %s: it cannot be restored on another node", backend, vol.Name)
		}
	}
	return nil
}

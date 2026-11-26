// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type runtimeVolumePublisher func(context.Context, *storage.PluginVolumeBackendInfo) error

// The resolver verifies the requested binding against the current execution
// and supplies its publication boundary. Update holds the lifecycle lock
// through resolution, storage preparation and the call to the publisher.
type runtimeVolumeAttachResolver func(context.Context, *cubebox.UpdateCubeSandboxRequest, *cubeboxstore.CubeBox) (volume.AttachRequest, runtimeVolumePublisher, error)

func (s *service) attachRuntimeVolumeLocked(ctx context.Context, req *cubebox.UpdateCubeSandboxRequest, rsp *cubebox.UpdateCubeSandboxResponse) *cubebox.UpdateCubeSandboxResponse {
	rsp.Ret.RetCode = errorcode.ErrorCode_Conflict
	if s.resolveRuntimeVolumeAttach == nil {
		rsp.Ret.RetMsg = "runtime volume publication is unavailable"
		return rsp
	}
	if err := ctx.Err(); err != nil {
		rsp.Ret.RetMsg = err.Error()
		return rsp
	}
	if req.GetRequestID() == "" {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = "runtime volume attach requires a request ID"
		return rsp
	}
	sb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, req.GetSandboxID())
	if err != nil || sb == nil {
		rsp.Ret.RetMsg = "runtime volume attach requires an existing sandbox"
		return rsp
	}
	current := sb.GetStatus().Get()
	if current.State() != cubebox.ContainerState_CONTAINER_RUNNING || current.Removing || current.RollingBack || sb.UserMarkDeletedTime != nil {
		rsp.Ret.RetMsg = "runtime volume attach requires a running sandbox outside a lifecycle transition"
		return rsp
	}
	source, publish, err := s.resolveRuntimeVolumeAttach(ctx, req, sb)
	if err != nil || publish == nil {
		rsp.Ret.RetMsg = "runtime volume attach binding or publication is unavailable"
		return rsp
	}
	source.SandboxID = sb.ID
	source.Namespace = sb.Namespace
	backend, err := storage.PrepareRuntimeVolumeBackend(ctx, source)
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = "runtime volume backend preparation failed"
		return rsp
	}

	// Keep the prepared backend on cancellation or an uncertain publication
	// outcome. Only a verified publication can support a successful reply.
	rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
	rsp.Ret.RetMsg = "runtime volume publication remains unconfirmed"
	if ctx.Err() == nil {
		if err := publish(ctx, backend); err != nil {
			rsp.Ret.RetMsg = "runtime volume publication ended without confirmation"
		}
	}
	return rsp
}

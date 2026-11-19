// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/admission"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/dao"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	codes "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"gorm.io/gorm"
)

type volumeSource struct {
	Name        string `json:"name"`
	Driver      string `json:"driver"`
	PrivateData string `json:"private_data,omitempty"`
}

// createWithVolumeAdmission covers both normal create and thin Pause Resume
// (whose volume IDs are saved in the pause binding rather than the request).
func createWithVolumeAdmission(ctx context.Context, endpoint string, req *cubebox.RunCubeSandboxRequest, savedIDs []string) (*cubebox.RunCubeSandboxResponse, bool, error) {
	var sources []volumeSource
	if raw := req.GetAnnotations()[AnnotationPluginVolumeSources]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &sources); err != nil {
			return nil, true, ret.Err(errorcode.ErrorCode_MasterParamsError, "invalid resolved plugin volumes")
		}
	}
	ids := append([]string(nil), savedIDs...)
	for _, src := range sources {
		ids = append(ids, src.Name)
	}
	for _, v := range req.GetVolumes() {
		if pv := v.GetVolumeSource().GetPluginVolume(); pv != nil {
			ids = append(ids, v.GetName())
		}
	}
	if len(ids) == 0 {
		rsp, err := cubelet.Create(ctx, endpoint, req)
		return rsp, false, err
	}

	var guard *admission.CreateGuard
	admissionAttempted := false
	rsp, err := cubelet.CreatePrepared(ctx, endpoint, req, func() error {
		admissionAttempted = true
		attempt, rows, err := admission.BeginCreate(ctx, dao.Default(), ids)
		switch {
		case errors.Is(err, admission.ErrBusy):
			return ret.Err(errorcode.ErrorCode_Conflict, err.Error())
		case errors.Is(err, gorm.ErrRecordNotFound):
			return ret.Err(errorcode.ErrorCode_NotFound, "plugin volume not found")
		case err != nil:
			return ret.Err(errorcode.ErrorCode_DBError, "could not reserve plugin volumes")
		}
		guard = attempt
		sources = sources[:0]
		for _, row := range rows {
			sources = append(sources, volumeSource{Name: row.VolumeID, Driver: row.Driver, PrivateData: row.PrivateData})
		}
		// volumeSource has only strings; this encoding cannot fail.
		data, _ := json.Marshal(sources)
		if req.Annotations == nil {
			req.Annotations = make(map[string]string)
		}
		req.Annotations[AnnotationPluginVolumeSources] = string(data)
		return nil
	})
	if err != nil {
		// A connection failure before the admission hook leaves nothing to
		// settle and can follow the ordinary scheduler retry path.
		return rsp, admissionAttempted, err
	}
	if rsp.GetRet() == nil {
		return rsp, true, nil
	}
	success := rsp.GetRet().GetRetCode() == codes.ErrorCode_Success
	ext := rsp.GetExtInfo()
	rolledBack := !success && string(ext[cubebox.ExtVolumeCreateRolledBack]) == "true"
	if !success && !rolledBack && string(ext[cubebox.ExtVolumeAttachNotStarted]) != "true" {
		return rsp, true, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	var events map[string][]byte
	if success || rolledBack {
		events = ext
	}
	if err := guard.Finish(ctx, events, rolledBack); err != nil {
		return rsp, true, ret.Err(errorcode.ErrorCode_DBError, "could not settle plugin volume reservation")
	}
	return rsp, true, nil
}

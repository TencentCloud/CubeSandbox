// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
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

type volumeCreateResult struct {
	rsp              *cubebox.RunCubeSandboxResponse
	guarded          bool
	failureSettled   bool
	createdSandboxID string
}

func matchesCreateResponse(req *cubebox.RunCubeSandboxRequest, rsp *cubebox.RunCubeSandboxResponse) bool {
	desired := req.GetAnnotations()[constants.CubeAnnotationDesiredSandboxID]
	got := rsp.GetSandboxID()
	return rsp.GetRequestID() == req.GetRequestID() && (desired == "" || got == "" || desired == got)
}

func volumeCreateReportFailed(rsp *cubebox.RunCubeSandboxResponse) bool {
	return rsp.GetRet() != nil && rsp.GetRet().GetRetCode() == codes.ErrorCode_Unknown &&
		string(rsp.GetExtInfo()[cubebox.ExtVolumeCreateReportFailed]) == "true"
}

func createResultFromRPC(req *cubebox.RunCubeSandboxRequest, rsp *cubebox.RunCubeSandboxResponse, rpcErr error) volumeCreateResult {
	if rpcErr != nil {
		return volumeCreateResult{}
	}
	result := volumeCreateResult{rsp: rsp}
	if rsp.GetRet() != nil && matchesCreateResponse(req, rsp) && strings.TrimSpace(rsp.GetSandboxID()) != "" &&
		(rsp.GetRet().GetRetCode() == codes.ErrorCode_Success || volumeCreateReportFailed(rsp)) {
		result.createdSandboxID = rsp.GetSandboxID()
	}
	return result
}

// createWithVolumeAdmission covers normal Create and thin Pause Resume, which
// also supplies volume IDs from the Pause binding and confirmed mount records.
func createWithVolumeAdmission(ctx context.Context, endpoint string, req *cubebox.RunCubeSandboxRequest, savedIDs []string, expectedRows map[string]uint) (volumeCreateResult, error) {
	var sources []volumeSource
	if raw := req.GetAnnotations()[AnnotationPluginVolumeSources]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &sources); err != nil {
			return volumeCreateResult{guarded: true}, ret.Err(errorcode.ErrorCode_MasterParamsError, "invalid resolved plugin volumes")
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
	if len(ids) == 0 && len(expectedRows) == 0 {
		rsp, err := cubelet.Create(ctx, endpoint, req)
		return createResultFromRPC(req, rsp, err), err
	}

	var guard *admission.CreateGuard
	admissionAttempted := false
	rsp, err := cubelet.CreatePrepared(ctx, endpoint, req, func() error {
		admissionAttempted = true
		attempt, rows, err := admission.BeginCreateWithExpectedRows(ctx, dao.Default(), ids, expectedRows)
		switch {
		case errors.Is(err, admission.ErrBusy), errors.Is(err, admission.ErrVolumeReplaced):
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
	result := createResultFromRPC(req, rsp, err)
	result.guarded = admissionAttempted
	// A connection failure before the admission hook leaves nothing to settle
	// and can follow the ordinary scheduler retry path.
	if err != nil || rsp.GetRet() == nil || volumeCreateReportFailed(rsp) {
		return result, err
	}
	code := rsp.GetRet().GetRetCode()
	success := code == codes.ErrorCode_Success
	ext := rsp.GetExtInfo()
	rolledBack := !success && string(ext[cubebox.ExtVolumeCreateRolledBack]) == "true"
	notStarted := !success && string(ext[cubebox.ExtVolumeAttachNotStarted]) == "true"
	if !success && !rolledBack && !notStarted {
		return result, nil
	}
	if !matchesCreateResponse(req, rsp) || (success && result.createdSandboxID == "") {
		return result, ret.Err(errorcode.ErrorCode_Unknown, "plugin volume Create response identity mismatch")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	var events map[string][]byte
	if success || rolledBack {
		events = ext
	}
	if err := guard.Finish(ctx, events, rolledBack); err != nil {
		return result, ret.Err(errorcode.ErrorCode_DBError, "could not settle plugin volume reservation")
	}
	result.failureSettled = !success
	return result, nil
}

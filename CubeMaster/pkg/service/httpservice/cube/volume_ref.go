// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"github.com/gin-gonic/gin"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"gorm.io/gorm"
)

// Direct /volumecontent access has no sandbox, so it cannot report node
// reference transitions on a sandbox create/destroy response. A Cubelet that
// serves content requests for a volume registers one reference here for as long
// as it has requests in flight, and gives it back afterwards. The reference is
// the same t_cube_volume.refcount that sandboxes maintain, so DELETE
// /cube/volume/{id} refuses a volume that is in use either way.
//
// The counter is anonymous (it records no holder), so these calls are not
// idempotent. Callers must not retry a call whose outcome is unknown.

type volumeRefRes struct {
	Ret      *types.Ret `json:"ret,omitempty"`
	VolumeID string     `json:"volumeID,omitempty"`
}

// handleAcquireVolumeRef implements POST /cube/volume/{id}/ref.
//
// It adds one reference if, and only if, the volume exists. Existence and the
// increment are one statement, and DELETE holds the volume row lock while it
// checks the count and removes the volume, so a reference is either seen by
// DELETE or rejected here with NotFound; it can never be added to a volume that
// is being deleted.
func handleAcquireVolumeRef(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	volumeID := c.Param("volume_id")
	if volumeID == "" {
		common.WriteAPI(c, &volumeRefRes{Ret: volErr(rt, errorcode.ErrorCode_MasterParamsError, "volumeID is required")})
		return
	}
	res := volumeDB().WithContext(c.Request.Context()).
		Model(&models.VolumeRecord{}).
		Where("volume_id = ?", volumeID).
		Update("refcount", gorm.Expr("refcount + ?", 1))
	switch {
	case res.Error != nil:
		common.WriteAPI(c, &volumeRefRes{Ret: volErr(rt, errorcode.ErrorCode_DBError, "db error: "+res.Error.Error())})
	case res.RowsAffected == 0:
		common.WriteAPI(c, &volumeRefRes{Ret: volErr(rt, errorcode.ErrorCode_NotFound, "volume not found: "+volumeID)})
	default:
		rt.RetCode = 0
		common.WriteAPI(c, &volumeRefRes{Ret: retOK(), VolumeID: volumeID})
	}
}

// handleReleaseVolumeRef implements DELETE /cube/volume/{id}/ref.
//
// It gives back one reference. The count never goes below zero, and releasing
// for a volume that no longer exists succeeds, so a late release is harmless.
// The guard is in the WHERE clause rather than a GREATEST() call so it behaves
// the same on MySQL, PostgreSQL and SQLite.
func handleReleaseVolumeRef(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	volumeID := c.Param("volume_id")
	if volumeID == "" {
		common.WriteAPI(c, &volumeRefRes{Ret: volErr(rt, errorcode.ErrorCode_MasterParamsError, "volumeID is required")})
		return
	}
	if err := volumeDB().WithContext(c.Request.Context()).
		Model(&models.VolumeRecord{}).
		Where("volume_id = ? AND refcount > 0", volumeID).
		Update("refcount", gorm.Expr("refcount - ?", 1)).Error; err != nil {
		common.WriteAPI(c, &volumeRefRes{Ret: volErr(rt, errorcode.ErrorCode_DBError, "db error: "+err.Error())})
		return
	}
	rt.RetCode = 0
	common.WriteAPI(c, &volumeRefRes{Ret: retOK(), VolumeID: volumeID})
}

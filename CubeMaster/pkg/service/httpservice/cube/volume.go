// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/plugin"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
	"gorm.io/gorm"
)

// ─── Request / response types ───────────────────────────────────────────────

// CreateVolumeReq is the JSON body expected by POST /cube/volume.
type CreateVolumeReq struct {
	// Name is the human-readable label for the new volume. Required.
	Name string `json:"name"`
	// Driver is the plugin name to use (e.g. "cos"). Optional;
	// defaults to the first registered plugin when empty.
	Driver string `json:"driver,omitempty"`
}

// VolumeItem is the serialised form of a volume returned to the caller.
type VolumeItem struct {
	VolumeID  string `json:"volumeID"`
	Name      string `json:"name"`
	Driver    string `json:"driver"`
	Token     string `json:"token,omitempty"`
	RefCount  int64  `json:"refCount"`
	CreatedAt int64  `json:"createdAt"`
}

// listVolumesRes is the response body for GET /cube/volume.
type listVolumesRes struct {
	Ret     *types.Ret   `json:"ret,omitempty"`
	Volumes []VolumeItem `json:"volumes"`
}

// singleVolumeRes is the response body for POST /cube/volume and
// GET /cube/volume/{id}.
type singleVolumeRes struct {
	Ret    *types.Ret  `json:"ret,omitempty"`
	Volume *VolumeItem `json:"volume,omitempty"`
}

// deleteVolumeRes is the response body for DELETE /cube/volume/{id}.
type deleteVolumeRes struct {
	Ret      *types.Ret `json:"ret,omitempty"`
	VolumeID string     `json:"volumeID,omitempty"`
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func retOK() *types.Ret { return &types.Ret{RetCode: 0, RetMsg: "ok"} }

// volErr builds an error Ret using a standard errorcode and records the same
// code on the request trace, so CubeAPI can map it to the correct HTTP status
// (e.g. NotFound→404, Conflict→409, MasterParamsError→400) instead of treating
// every failure as an internal error.
func volErr(rt *CubeLog.RequestTrace, code errorcode.ErrorCode, msg string) *types.Ret {
	rt.RetCode = int64(code)
	return &types.Ret{RetCode: int(code), RetMsg: msg}
}

// volumeItemFromRecord converts a DB row to the wire type.
func volumeItemFromRecord(r *models.VolumeRecord) VolumeItem {
	return VolumeItem{
		VolumeID:  r.VolumeID,
		Name:      r.Name,
		Driver:    r.Driver,
		Token:     r.Token,
		RefCount:  r.RefCount,
		CreatedAt: r.CreatedAt.Unix(),
	}
}

// ─── Handlers ───────────────────────────────────────────────────────────────

// handleListVolumes implements GET /cube/volume
func handleListVolumes(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	var records []models.VolumeRecord
	if err := dao.Default().
		WithContext(c.Request.Context()).
		Find(&records).Error; err != nil {
		common.WriteAPI(c, &listVolumesRes{Ret: volErr(rt, errorcode.ErrorCode_DBError, "db error: "+err.Error()), Volumes: []VolumeItem{}})
		return
	}
	items := make([]VolumeItem, 0, len(records))
	for i := range records {
		items = append(items, volumeItemFromRecord(&records[i]))
	}
	rt.RetCode = 0
	common.WriteAPI(c, &listVolumesRes{Ret: retOK(), Volumes: items})
}

// handleCreateVolume implements POST /cube/volume
func handleCreateVolume(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	var req CreateVolumeReq
	if err := common.GetBodyReq(c.Request, &req); err != nil {
		common.WriteAPI(c, &singleVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_MasterParamsError, "invalid request: "+err.Error())})
		return
	}

	// Unify name and volume_id:
	// - If caller supplies a name, use it as both the human label AND the stable ID.
	// - If caller omits a name, generate a UUIDv4 and use it for both fields.
	// This ensures VolumeSpec.name (forwarded to CubeMaster during sandbox create)
	// can be used directly as the volume_id lookup key.
	req.Name = strings.TrimSpace(req.Name)
	var volumeID string
	if req.Name == "" {
		generated := uuid.New().String()
		volumeID = generated
		req.Name = generated
	} else if len(req.Name) > models.MaxVolumeNameLen {
		common.WriteAPI(c, &singleVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_MasterParamsError, fmt.Sprintf(
			"volume name exceeds max length %d", models.MaxVolumeNameLen,
		))})
		return
	} else if !isValidVolumeName(req.Name) {
		common.WriteAPI(c, &singleVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_MasterParamsError,
			"volume name must match ^[a-zA-Z0-9_-]+$")})
		return
	} else {
		volumeID = req.Name
	}

	if req.Driver == "" {
		p, ok := plugin.First()
		if !ok {
			common.WriteAPI(c, &singleVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_MasterInternalError, "no volume plugin registered")})
			return
		}
		req.Driver = p.Name()
	}

	p, ok := plugin.Get(req.Driver)
	if !ok {
		common.WriteAPI(c, &singleVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_MasterParamsError, "unknown driver: "+req.Driver)})
		return
	}

	// Reserve the DB row first so concurrent creates for the same name/id are
	// decided by UNIQUE(volume_id)/UNIQUE(name). Only the winner may call the
	// plugin; losers must NOT Destroy — the backend belongs to the winner.
	record := &models.VolumeRecord{
		VolumeID: volumeID,
		Name:     req.Name,
		Driver:   req.Driver,
	}
	if err := dao.Default().WithContext(c.Request.Context()).Create(record).Error; err != nil {
		if isDuplicateKey(err) {
			common.WriteAPI(c, &singleVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_Conflict, "volume already exists: "+volumeID)})
			return
		}
		common.WriteAPI(c, &singleVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_DBError, "db create error: "+err.Error())})
		return
	}

	info, err := p.Create(c.Request.Context(), volumeID, req.Name)
	if err != nil {
		_ = dao.Default().WithContext(c.Request.Context()).
			Where("volume_id = ?", volumeID).
			Delete(&models.VolumeRecord{}).Error
		common.WriteAPI(c, &singleVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_MasterInternalError, "plugin create error: "+err.Error())})
		return
	}

	if info.Token != "" {
		record.Token = info.Token
		if err := dao.Default().WithContext(c.Request.Context()).
			Model(record).
			Update("token", info.Token).Error; err != nil {
			_ = p.Destroy(c.Request.Context(), volumeID)
			_ = dao.Default().WithContext(c.Request.Context()).
				Where("volume_id = ?", volumeID).
				Delete(&models.VolumeRecord{}).Error
			common.WriteAPI(c, &singleVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_DBError, "db update token error: "+err.Error())})
			return
		}
	}

	item := volumeItemFromRecord(record)
	rt.RetCode = 0
	common.WriteAPI(c, &singleVolumeRes{Ret: retOK(), Volume: &item})
}

// handleGetVolume implements GET /cube/volume/{id}
func handleGetVolume(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	volumeID := c.Param("volume_id")
	if volumeID == "" {
		common.WriteAPI(c, &singleVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_MasterParamsError, "volumeID is required")})
		return
	}

	var record models.VolumeRecord
	err := dao.Default().
		WithContext(c.Request.Context()).
		Where("volume_id = ?", volumeID).
		First(&record).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			common.WriteAPI(c, &singleVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_NotFound, "volume not found: "+volumeID)})
			return
		}
		common.WriteAPI(c, &singleVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_DBError, "db error: "+err.Error())})
		return
	}

	item := volumeItemFromRecord(&record)
	rt.RetCode = 0
	common.WriteAPI(c, &singleVolumeRes{Ret: retOK(), Volume: &item})
}

// handleDeleteVolume implements DELETE /cube/volume/{id}
func handleDeleteVolume(c *gin.Context) {
	rt := CubeLog.GetTraceInfo(c.Request.Context())
	volumeID := c.Param("volume_id")
	if volumeID == "" {
		common.WriteAPI(c, &deleteVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_MasterParamsError, "volumeID is required")})
		return
	}

	var record models.VolumeRecord
	err := dao.Default().
		WithContext(c.Request.Context()).
		Where("volume_id = ?", volumeID).
		First(&record).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			common.WriteAPI(c, &deleteVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_NotFound, "volume not found: "+volumeID)})
			return
		}
		common.WriteAPI(c, &deleteVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_DBError, "db error: "+err.Error())})
		return
	}

	// Refuse to delete a volume that is still attached on one or more nodes.
	// RefCount is maintained from the node-level transitions Cubelet reports on
	// sandbox create/destroy; a non-zero value means live sandboxes still use it.
	if record.RefCount != 0 {
		common.WriteAPI(c, &deleteVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_Conflict, fmt.Sprintf(
			"volume %s is in use by %d node(s); destroy the sandboxes using it before deleting",
			volumeID, record.RefCount,
		))})
		return
	}

	p, ok := plugin.Get(record.Driver)
	if !ok {
		common.WriteAPI(c, &deleteVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_MasterParamsError, "unknown driver: "+record.Driver)})
		return
	}

	if destroyErr := p.Destroy(c.Request.Context(), volumeID); destroyErr != nil {
		common.WriteAPI(c, &deleteVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_MasterInternalError, "plugin destroy error: "+destroyErr.Error())})
		return
	}

	// Hard-delete: remove the row so volume_id / name can be reused.
	if dbErr := dao.Default().WithContext(c.Request.Context()).Delete(&record).Error; dbErr != nil {
		common.WriteAPI(c, &deleteVolumeRes{Ret: volErr(rt, errorcode.ErrorCode_DBError, "db delete error: "+dbErr.Error())})
		return
	}

	rt.RetCode = 0
	common.WriteAPI(c, &deleteVolumeRes{Ret: retOK(), VolumeID: volumeID})
}

// isValidVolumeName matches CubeAPI's NewVolume::name_is_valid:
// non-empty names must be ^[a-zA-Z0-9_-]+$ (length checked separately).
func isValidVolumeName(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return name != ""
}

// isDuplicateKey reports whether err is a unique-constraint violation from
// MySQL (1062) / PostgreSQL / GORM's ErrDuplicatedKey.
func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "1062") ||
		strings.Contains(msg, "Duplicate entry") ||
		strings.Contains(msg, "UNIQUE constraint") ||
		strings.Contains(msg, "duplicate key")
}

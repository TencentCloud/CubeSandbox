// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

// SnapshotReferencesPluginVolume reports whether a snapshot that can still
// become READY or be restored references volumeID. Reference-only snapshots
// retain the backing volume but need no provider-side snapshot object.
func SnapshotReferencesPluginVolume(ctx context.Context, volumeID string) (bool, error) {
	if !isReady() {
		return false, ErrTemplateStoreNotInitialized
	}
	volumeID = strings.TrimSpace(volumeID)
	if volumeID == "" {
		return false, nil
	}
	encodedVolumeID, err := json.Marshal(volumeID)
	if err != nil {
		return false, err
	}
	quotedVolumeID := string(encodedVolumeID)
	var rows []struct {
		RequestJSON string `gorm:"column:request_json"`
	}
	if err := store.db.WithContext(ctx).
		Table(constants.SnapshotTableName).
		Select("request_json").
		Where("status IN ?", []string{StatusCreating, StatusReady}).
		Where("request_json LIKE ?", "%"+quotedVolumeID+"%").
		Find(&rows).Error; err != nil {
		return false, err
	}
	for _, row := range rows {
		// LIKE may over-match '_' in a volume ID. Check the exact JSON string
		// before parsing so an unrelated malformed snapshot cannot block delete.
		if !strings.Contains(row.RequestJSON, quotedVolumeID) {
			continue
		}
		req, err := requestFromSnapshotJSON(row.RequestJSON)
		if err != nil {
			return false, fmt.Errorf("parse snapshot volume dependency: %w", err)
		}
		referenced, err := createRequestReferencesPluginVolume(req, volumeID)
		if err != nil {
			return false, err
		}
		if referenced {
			return true, nil
		}
	}
	return false, nil
}

func createRequestReferencesPluginVolume(req *sandboxtypes.CreateCubeSandboxReq, volumeID string) (bool, error) {
	if req == nil {
		return false, nil
	}
	type namedEntry struct {
		Name string `json:"name"`
	}
	for _, key := range []string{"plugin-volume-mounts", "plugin-volume-sources"} {
		raw := strings.TrimSpace(req.Annotations[key])
		if raw == "" || raw == "[]" || strings.EqualFold(raw, "null") {
			continue
		}
		var entries []namedEntry
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			return false, fmt.Errorf("parse %s snapshot dependency: %w", key, err)
		}
		for _, entry := range entries {
			if strings.TrimSpace(entry.Name) == volumeID {
				return true, nil
			}
		}
	}
	for _, volume := range req.Volumes {
		if volume == nil || strings.TrimSpace(volume.Name) != volumeID {
			continue
		}
		if volume.VolumeSource == nil || volume.VolumeSource.PluginVolume != nil {
			return true, nil
		}
	}
	return false, nil
}

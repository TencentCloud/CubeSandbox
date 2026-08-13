// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"fmt"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"
	dbmodels "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
)

func TestValidatePauseResumeVolumesEmptyOK(t *testing.T) {
	t.Parallel()
	require.NoError(t, validatePauseResumeVolumes(nil))
	require.NoError(t, validatePauseResumeVolumes([]string{"", "  "}))
}

func TestValidatePauseResumeVolumesMissing(t *testing.T) {
	patches := gomonkey.ApplyFunc(resolveVolumeRecord, func(volumeID string) (*dbmodels.VolumeRecord, error) {
		return nil, fmt.Errorf("volume not found: %s", volumeID)
	})
	defer patches.Reset()

	err := validatePauseResumeVolumes([]string{"vol-gone"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot resume")
	require.Contains(t, err.Error(), "vol-gone")
}

func TestValidatePauseResumeVolumesPresent(t *testing.T) {
	patches := gomonkey.ApplyFunc(resolveVolumeRecord, func(volumeID string) (*dbmodels.VolumeRecord, error) {
		return &dbmodels.VolumeRecord{VolumeID: volumeID}, nil
	})
	defer patches.Reset()

	require.NoError(t, validatePauseResumeVolumes([]string{"vol-ok", "  vol-ok2  "}))
}

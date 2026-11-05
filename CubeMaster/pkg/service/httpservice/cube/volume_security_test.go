// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	masterlog "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"gorm.io/gorm"
)

func TestVolumeHTTP_InvalidPrivateDataRequestDoesNotEchoInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{"object", `{"marker":"private-data-sentinel"}`},
		{"array", `["private-data-sentinel"]`},
		{"invalid_escape", `"private-data-sentinel\q"`},
		{"truncated", `"private-data-sentinel"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, db, fake := newVolumeTestEngine(t)
			reservations := 0
			const callback = "test:count_invalid_request_reservations"
			require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == models.VolumeTableName {
					reservations++
				}
			}))
			t.Cleanup(func() { require.NoError(t, db.Callback().Create().Remove(callback)) })
			body := fmt.Sprintf(`{"name":"vol-invalid-input","driver":%q,"private_data":%s}`, fake.Name(), tc.data)
			if tc.name == "truncated" {
				body = strings.TrimSuffix(body, "}")
			}
			w, result := doVolumeRequest(t, r, http.MethodPost, "/cube/volume", body)
			assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), result.Ret.RetCode)
			assert.Equal(t, "invalid request", result.Ret.RetMsg)
			assert.NotContains(t, w.Body.String(), "private-data-sentinel")
			assert.Zero(t, reservations)
			assert.Zero(t, fake.createCalls)
			assert.Zero(t, fake.destroyCalls)
			var count int64
			require.NoError(t, db.Model(&models.VolumeRecord{}).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
}

func TestVolumeHTTP_PrivateDataNULInputRejectedBeforeReservation(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	reservations := 0
	const callback = "test:count_nul_input_reservations"
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == models.VolumeTableName {
			reservations++
		}
	}))
	t.Cleanup(func() { require.NoError(t, db.Callback().Create().Remove(callback)) })
	_, result := doVolumeRequestWithStatus(t, r, http.MethodPost, "/cube/volume", volumeCreateBody(t, "vol-nul-input", fake.Name(), "sentinel-before\x00sentinel-after"), http.StatusBadRequest)
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), result.Ret.RetCode)
	assert.Contains(t, result.Ret.RetMsg, "NUL")
	assert.NotContains(t, result.Ret.RetMsg, "sentinel")
	assert.Zero(t, reservations)
	assert.Zero(t, fake.createCalls)
}

func TestVolumeHTTP_PrivateDataNonBMPInputRejectedBeforeReservation(t *testing.T) {
	for _, input := range []string{"before😀after", "𠀀"} {
		t.Run(input, func(t *testing.T) {
			r, db, fake := newVolumeTestEngine(t)
			reservations := 0
			const callback = "test:count_non_bmp_input_reservations"
			require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == models.VolumeTableName {
					reservations++
				}
			}))
			t.Cleanup(func() { require.NoError(t, db.Callback().Create().Remove(callback)) })
			_, result := doVolumeRequestWithStatus(t, r, http.MethodPost, "/cube/volume",
				volumeCreateBody(t, "vol-non-bmp-input", fake.Name(), input), http.StatusBadRequest)
			assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), result.Ret.RetCode)
			assert.Contains(t, result.Ret.RetMsg, "outside the BMP")
			assert.NotContains(t, result.Ret.RetMsg, input)
			assert.Zero(t, reservations)
			assert.Zero(t, fake.createCalls)
			assert.Zero(t, fake.destroyCalls)
			var count int64
			require.NoError(t, db.Model(&models.VolumeRecord{}).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
}

func TestVolumeHTTP_PrivateDataBMPBoundaryStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		input  string
		status int
	}{
		{"last_bmp", "\uFFFF", http.StatusOK},
		{"first_supplementary", "\U00010000", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, db, fake := newVolumeTestEngine(t)
			const volumeID = "vol-bmp-boundary"
			w, result := doVolumeRequestWithStatus(t, r, http.MethodPost, "/cube/volume",
				volumeCreateBody(t, volumeID, fake.Name(), tc.input), tc.status)
			assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
			var count int64
			require.NoError(t, db.Model(&models.VolumeRecord{}).Count(&count).Error)
			if tc.status == http.StatusOK {
				assert.Zero(t, result.Ret.RetCode)
				assert.Equal(t, 1, fake.createCalls)
				assert.Equal(t, tc.input, fake.createInputs[volumeID])
				assert.EqualValues(t, 1, count)
			} else {
				assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), result.Ret.RetCode)
				assert.Contains(t, result.Ret.RetMsg, "outside the BMP")
				assert.Zero(t, fake.createCalls)
				assert.Zero(t, count)
			}
			assert.Zero(t, fake.destroyCalls)
		})
	}
}

func TestVolumeHTTP_PrivateDataNULOutputRollsBackReturnedState(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	fake.privateData = "returned-before\x00returned-after"
	_, result := doVolumeRequestWithStatus(t, r, http.MethodPost, "/cube/volume", volumeCreateBody(t, "vol-nul-state", fake.Name(), "valid-input"), http.StatusBadRequest)
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), result.Ret.RetCode)
	assert.Contains(t, result.Ret.RetMsg, "NUL")
	assert.NotContains(t, result.Ret.RetMsg, "returned-before")
	assert.Equal(t, 1, fake.destroyCalls)
	assert.Equal(t, fake.privateData, fake.destroyInputs["vol-nul-state"])
	var count int64
	require.NoError(t, db.Model(&models.VolumeRecord{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestVolumeHTTP_PrivateDataNonBMPOutputRollsBackCompleteReturnedState(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	fake.privateData = "state-B-😀-must-be-preserved"
	_, result := doVolumeRequestWithStatus(t, r, http.MethodPost, "/cube/volume",
		volumeCreateBody(t, "vol-non-bmp-state", fake.Name(), "valid-input-A"), http.StatusBadRequest)
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), result.Ret.RetCode)
	assert.Contains(t, result.Ret.RetMsg, "outside the BMP")
	assert.NotContains(t, result.Ret.RetMsg, "state-B")
	assert.Equal(t, 1, fake.destroyCalls)
	assert.Equal(t, fake.privateData, fake.destroyInputs["vol-non-bmp-state"], "rollback must receive complete B")
	var count int64
	require.NoError(t, db.Model(&models.VolumeRecord{}).Count(&count).Error)
	assert.Zero(t, count)
}

type volumeLogSink chan string

func (sink volumeLogSink) Write(data []byte) (int, error) {
	sink <- string(data)
	return len(data), nil
}

func TestVolumeRollbackDiagnosticsDoNotEchoPluginError(t *testing.T) {
	for _, tc := range []struct {
		name, class string
		err         error
	}{
		{"plugin", "plugin_failure", errors.New("raw-plugin-error-state-B-sentinel")},
		{"canceled", "canceled", fmt.Errorf("state-B-sentinel: %w", context.Canceled)},
		{"timeout", "timeout", fmt.Errorf("state-B-sentinel: %w", context.DeadlineExceeded)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeControllerPlugin("fixture-driver")
			fake.destroyErr = tc.err
			sink := make(volumeLogSink, 4)
			logger := CubeLog.GetLogger(t.Name())
			logger.SetOutput(sink)
			ctx := masterlog.WithLogger(context.Background(), CubeLog.NewEntry(logger))
			rollbackCreatedVolume(ctx, fake, "vol-cleanup", "state-B-sentinel", "db_update_failed")
			select {
			case message := <-sink:
				for _, value := range []string{"vol-cleanup", "fixture-driver", "db_update_failed", tc.class} {
					assert.Contains(t, message, value)
				}
				assert.NotContains(t, message, "state-B-sentinel")
				assert.NotContains(t, message, "raw-plugin-error")
			case <-time.After(3 * time.Second):
				t.Fatal("cleanup failure diagnostic not emitted")
			}
		})
	}
}

func TestVolumeHTTP_RollbackFailureStillDeletesReservation(t *testing.T) {
	for _, phase := range []string{"invalid_state", "update_failed"} {
		t.Run(phase, func(t *testing.T) {
			r, db, fake := newVolumeTestEngine(t)
			fake.privateData = "plugin-state-B"
			fake.destroyErr = errors.New("plugin-state-B cleanup failed")
			if phase == "invalid_state" {
				fake.privateData = strings.Repeat("b", models.MaxPrivateDataLen+1)
			} else {
				const callback = "test:fail_update_and_cleanup"
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table == models.VolumeTableName {
						tx.AddError(errors.New("injected update failure"))
					}
				}))
				t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove(callback)) })
			}
			wantStatus := http.StatusOK
			wantCode := errorcode.ErrorCode_DBError
			if phase == "invalid_state" {
				wantStatus = http.StatusBadRequest
				wantCode = errorcode.ErrorCode_MasterParamsError
			}
			_, result := doVolumeRequestWithStatus(t, r, http.MethodPost, "/cube/volume",
				volumeCreateBody(t, "vol-cleanup-failure", fake.Name(), "caller-input-A"), wantStatus)
			assert.Equal(t, int(wantCode), result.Ret.RetCode)
			assert.Equal(t, 1, fake.destroyCalls)
			assert.Equal(t, fake.privateData, fake.destroyInputs["vol-cleanup-failure"])
			var count int64
			require.NoError(t, db.Model(&models.VolumeRecord{}).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
}

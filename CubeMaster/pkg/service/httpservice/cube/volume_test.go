// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	// glebarez/sqlite is a pure-Go (CGO-free) SQLite driver, used here as an
	// in-memory DB so these tests exercise the REAL create→list→get→delete and
	// refcount round-trips against gorm. This intentionally extends the
	// "stub-only, deps frozen" convention documented in templatecenter's
	// store_test.go: sqlmock would force brittle ordered exact-SQL matching
	// against gorm internals for multi-statement handler flows. The driver is a
	// test-only import and is excluded from the shipped CubeMaster binary.
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/plugin"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// fakeControllerPlugin is an in-process ControllerPlugin used by volume HTTP
// tests. It does not touch COS or any external backend.
type fakeControllerPlugin struct {
	name string

	// createErr, when set, makes Create fail so tests can exercise the
	// handler's DB-row rollback path.
	createErr  error
	destroyErr error
	// privateData is the Create response; inputs are recorded independently.
	privateData string

	mu            sync.Mutex
	created       map[string]string // volumeID → name
	destroyed     map[string]bool
	createInputs  map[string]string // volumeID → caller's private_data
	destroyInputs map[string]string // volumeID → plugin's returned state
	createCalls   int
	destroyCalls  int
}

func newFakeControllerPlugin(name string) *fakeControllerPlugin {
	return &fakeControllerPlugin{
		name:          name,
		created:       map[string]string{},
		destroyed:     map[string]bool{},
		createInputs:  map[string]string{},
		destroyInputs: map[string]string{},
	}
}

func (f *fakeControllerPlugin) Name() string { return f.name }

func (f *fakeControllerPlugin) Create(_ context.Context, volumeID, name, privateData string) (*plugin.VolumeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.createInputs[volumeID] = privateData
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created[volumeID] = name
	return &plugin.VolumeInfo{
		VolumeID:    volumeID,
		Name:        name,
		Token:       "tok-" + volumeID,
		PrivateData: f.privateData,
		PluginName:  f.name,
	}, nil
}

func (f *fakeControllerPlugin) Destroy(_ context.Context, volumeID, privateData string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyCalls++
	f.destroyInputs[volumeID] = privateData
	f.destroyed[volumeID] = true
	if f.destroyErr != nil {
		return f.destroyErr
	}
	delete(f.created, volumeID)
	return nil
}

func (f *fakeControllerPlugin) wasCreated(volumeID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.created[volumeID]
	return ok
}

func (f *fakeControllerPlugin) wasDestroyed(volumeID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.destroyed[volumeID]
}

func newVolumeTestEngine(t *testing.T) (*gin.Engine, *gorm.DB, *fakeControllerPlugin) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dbName := fmt.Sprintf("file:volume_test_%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.VolumeRecord{}))

	origDB := volumeDB
	volumeDB = func() *gorm.DB { return db }
	t.Cleanup(func() {
		volumeDB = origDB
	})

	fake := newFakeControllerPlugin("fake-vol")
	plugin.Register(fake)
	t.Cleanup(func() { plugin.UnregisterForTest(fake.Name()) })

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(
			CubeLog.WithRequestTrace(c.Request.Context(), &CubeLog.RequestTrace{}))
		c.Next()
	})
	RegisterCubeRoutes(r.Group("/cube"))
	return r, db, fake
}

type volumeRetEnvelope struct {
	Ret *struct {
		RetCode int    `json:"ret_code"`
		RetMsg  string `json:"ret_msg"`
	} `json:"ret"`
	Volume   *VolumeItem  `json:"volume"`
	Volumes  []VolumeItem `json:"volumes"`
	VolumeID string       `json:"volumeID"`
}

func doVolumeRequest(t *testing.T, r *gin.Engine, method, path, body string) (*httptest.ResponseRecorder, volumeRetEnvelope) {
	t.Helper()
	return doVolumeRequestWithStatus(t, r, method, path, body, http.StatusOK)
}

func doVolumeRequestWithStatus(t *testing.T, r *gin.Engine, method, path, body string, wantStatus int) (*httptest.ResponseRecorder, volumeRetEnvelope) {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, wantStatus, w.Code, "unexpected HTTP status; body=%s", w.Body.String())

	var env volumeRetEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), "body=%s", w.Body.String())
	require.NotNil(t, env.Ret, "response must carry ret envelope: %s", w.Body.String())
	return w, env
}

func volumeCreateBody(t *testing.T, volumeID, driver, privateData string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"name": volumeID, "driver": driver, "private_data": privateData,
	})
	require.NoError(t, err)
	return string(body)
}

func assertVolumeResponseOmitsPrivateData(t *testing.T, w *httptest.ResponseRecorder, values ...string) {
	t.Helper()
	// Inspect the actual HTTP response: decoding into VolumeItem would silently
	// discard an accidentally exposed field before the test could detect it.
	raw := w.Body.String()
	assert.NotContains(t, raw, `"private_data"`)
	assert.NotContains(t, raw, `"privateData"`)
	for _, value := range values {
		if value != "" {
			assert.NotContains(t, raw, value)
		}
	}
}

func TestVolumeHTTP_PrivateDataMissingOrEmpty(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit_%t", explicit), func(t *testing.T) {
			r, db, fake := newVolumeTestEngine(t)
			const volumeID = "vol-empty-private-data"
			body := fmt.Sprintf(`{"name":%q,"driver":%q}`, volumeID, fake.Name())
			if explicit {
				body = volumeCreateBody(t, volumeID, fake.Name(), "")
			}
			_, created := doVolumeRequest(t, r, http.MethodPost, "/cube/volume", body)
			require.Zero(t, created.Ret.RetCode)
			require.Equal(t, 1, fake.createCalls)
			assert.Equal(t, "", fake.createInputs[volumeID])
			var record models.VolumeRecord
			require.NoError(t, db.Where("volume_id = ?", volumeID).First(&record).Error)
			assert.Empty(t, record.PrivateData)
			_, deleted := doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+volumeID, "")
			require.Zero(t, deleted.Ret.RetCode)
			require.Equal(t, 1, fake.destroyCalls)
			assert.Equal(t, "", fake.destroyInputs[volumeID])
		})
	}
}

func TestVolumeHTTP_PrivateDataInputBytes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		valid bool
	}{
		{"ascii_limit", strings.Repeat("a", models.MaxPrivateDataLen), true},
		{"ascii_over_limit", strings.Repeat("a", models.MaxPrivateDataLen+1), false},
		{"unicode_limit", strings.Repeat("界", models.MaxPrivateDataLen/3) + "a", true},
		{"unicode_over_limit", strings.Repeat("界", models.MaxPrivateDataLen/3) + "ab", false},
		{"opaque_characters", "  \t桶 '\" \\\\ $() ; \n ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, db, fake := newVolumeTestEngine(t)
			const volumeID = "vol-input-bytes"
			const callbackName = "test:count_volume_reservations"
			reservationCalls := 0
			require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
				if tx.Statement.Table == models.VolumeTableName {
					reservationCalls++
				}
			}))
			t.Cleanup(func() { require.NoError(t, db.Callback().Create().Remove(callbackName)) })
			wantStatus := http.StatusBadRequest
			if tc.valid {
				wantStatus = http.StatusOK
			}
			_, env := doVolumeRequestWithStatus(t, r, http.MethodPost, "/cube/volume",
				volumeCreateBody(t, volumeID, fake.Name(), tc.input), wantStatus)
			var count int64
			require.NoError(t, db.Model(&models.VolumeRecord{}).Count(&count).Error)
			if tc.valid {
				require.Zero(t, env.Ret.RetCode)
				assert.Equal(t, 1, reservationCalls)
				assert.Equal(t, 1, fake.createCalls)
				assert.Equal(t, tc.input, fake.createInputs[volumeID])
				assert.EqualValues(t, 1, count)
			} else {
				assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), env.Ret.RetCode)
				assert.Contains(t, env.Ret.RetMsg, "1024")
				assert.NotContains(t, env.Ret.RetMsg, tc.input)
				assert.NotContains(t, env.Ret.RetMsg, "plugin")
				assert.Zero(t, reservationCalls, "oversized input must be rejected before attempting a DB reservation")
				assert.Zero(t, fake.createCalls)
				assert.Zero(t, count, "oversized input must be rejected before reserving a row")
			}
			assert.Zero(t, fake.destroyCalls)
		})
	}
}

func TestVolumeHTTP_PrivateDataInputErrorTraceMatchesResponse(t *testing.T) {
	_, _, fake := newVolumeTestEngine(t)
	rt := &CubeLog.RequestTrace{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/cube/volume", strings.NewReader(
		volumeCreateBody(t, "vol-input-trace", fake.Name(), strings.Repeat("x", models.MaxPrivateDataLen+1))))
	c.Request = req.WithContext(CubeLog.WithRequestTrace(req.Context(), rt))
	handleCreateVolume(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	var env volumeRetEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	require.NotNil(t, env.Ret)
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), env.Ret.RetCode)
	assert.Equal(t, int64(env.Ret.RetCode), rt.RetCode)
	assert.Zero(t, fake.createCalls)
}

func TestVolumeHTTP_CRUDWithFakePlugin(t *testing.T) {
	r, _, fake := newVolumeTestEngine(t)
	volumeID := "vol-crud-1"

	_, created := doVolumeRequest(t, r, http.MethodPost, "/cube/volume",
		fmt.Sprintf(`{"name":%q,"driver":%q}`, volumeID, fake.Name()))
	assert.Equal(t, 0, created.Ret.RetCode)
	require.NotNil(t, created.Volume)
	assert.Equal(t, volumeID, created.Volume.VolumeID)
	assert.Equal(t, volumeID, created.Volume.Name)
	assert.Equal(t, fake.Name(), created.Volume.Driver)
	assert.Equal(t, "tok-"+volumeID, created.Volume.Token)
	assert.True(t, fake.wasCreated(volumeID))

	_, listed := doVolumeRequest(t, r, http.MethodGet, "/cube/volume", "")
	assert.Equal(t, 0, listed.Ret.RetCode)
	found := false
	for _, v := range listed.Volumes {
		if v.VolumeID == volumeID {
			found = true
			break
		}
	}
	assert.True(t, found, "created volume must appear in list")

	_, got := doVolumeRequest(t, r, http.MethodGet, "/cube/volume/"+volumeID, "")
	assert.Equal(t, 0, got.Ret.RetCode)
	require.NotNil(t, got.Volume)
	assert.Equal(t, "tok-"+volumeID, got.Volume.Token)

	_, deleted := doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+volumeID, "")
	assert.Equal(t, 0, deleted.Ret.RetCode)
	assert.Equal(t, volumeID, deleted.VolumeID)
	assert.True(t, fake.wasDestroyed(volumeID))

	_, missing := doVolumeRequest(t, r, http.MethodGet, "/cube/volume/"+volumeID, "")
	assert.Equal(t, int(errorcode.ErrorCode_NotFound), missing.Ret.RetCode)
}

func TestVolumeHTTP_DeleteWhenRefCountNonZeroReturnsConflict(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	volumeID := "vol-in-use"

	_, created := doVolumeRequest(t, r, http.MethodPost, "/cube/volume",
		fmt.Sprintf(`{"name":%q,"driver":%q}`, volumeID, fake.Name()))
	require.Equal(t, 0, created.Ret.RetCode)

	require.NoError(t, db.Model(&models.VolumeRecord{}).
		Where("volume_id = ?", volumeID).
		Update("refcount", 1).Error)

	_, env := doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+volumeID, "")
	assert.Equal(t, int(errorcode.ErrorCode_Conflict), env.Ret.RetCode)
	assert.Contains(t, env.Ret.RetMsg, "in use")
	assert.False(t, fake.wasDestroyed(volumeID), "plugin Destroy must not run while RefCount != 0")
	assert.Zero(t, fake.destroyCalls)

	// After refcount drops, delete succeeds.
	require.NoError(t, db.Model(&models.VolumeRecord{}).
		Where("volume_id = ?", volumeID).
		Update("refcount", 0).Error)
	_, env = doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+volumeID, "")
	assert.Equal(t, 0, env.Ret.RetCode)
	assert.True(t, fake.wasDestroyed(volumeID))
	assert.Equal(t, 1, fake.destroyCalls)
}

func TestVolumeHTTP_UnknownDriverRejected(t *testing.T) {
	r, _, _ := newVolumeTestEngine(t)
	_, env := doVolumeRequest(t, r, http.MethodPost, "/cube/volume",
		`{"name":"vol-bad-driver","driver":"no-such-driver"}`)
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), env.Ret.RetCode)
}

// When the plugin's Create fails, the handler must roll back the reserved DB
// row so the volume_id/name stays reusable.
func TestVolumeHTTP_PluginCreateFailureRollsBackRow(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	fake.createErr = errors.New("backend unavailable")
	volumeID := "vol-create-fail"

	_, env := doVolumeRequest(t, r, http.MethodPost, "/cube/volume",
		volumeCreateBody(t, volumeID, fake.Name(), "caller-input-A"))
	assert.Equal(t, int(errorcode.ErrorCode_MasterInternalError), env.Ret.RetCode)
	assert.Equal(t, 1, fake.createCalls)
	assert.Equal(t, "caller-input-A", fake.createInputs[volumeID])
	assert.Zero(t, fake.destroyCalls, "Create error must not trigger Destroy")

	var record models.VolumeRecord
	err := db.Where("volume_id = ?", volumeID).First(&record).Error
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound, "reserved row must be rolled back after plugin Create failure")
}

// Creating the same name twice must return Conflict, driven by the DB unique
// constraint via isDuplicateKey (pins the "UNIQUE constraint" branch on sqlite).
func TestVolumeHTTP_DuplicateNameReturnsConflict(t *testing.T) {
	r, _, fake := newVolumeTestEngine(t)
	body := fmt.Sprintf(`{"name":"vol-dup","driver":%q}`, fake.Name())

	_, first := doVolumeRequest(t, r, http.MethodPost, "/cube/volume", body)
	require.Equal(t, 0, first.Ret.RetCode)

	_, second := doVolumeRequest(t, r, http.MethodPost, "/cube/volume", body)
	assert.Equal(t, int(errorcode.ErrorCode_Conflict), second.Ret.RetCode)
	assert.Equal(t, 1, fake.createCalls, "the duplicate must not invoke Create")
	assert.Zero(t, fake.destroyCalls, "the duplicate does not own the backend")
}

// Omitting the name makes the server generate a UUID used for both name and
// volume_id, so VolumeSpec.name can be used directly as the lookup key.
func TestVolumeHTTP_EmptyNameGeneratesUUID(t *testing.T) {
	r, _, fake := newVolumeTestEngine(t)
	_, env := doVolumeRequest(t, r, http.MethodPost, "/cube/volume",
		fmt.Sprintf(`{"driver":%q}`, fake.Name()))
	require.Equal(t, 0, env.Ret.RetCode)
	require.NotNil(t, env.Volume)
	assert.NotEmpty(t, env.Volume.VolumeID)
	assert.Equal(t, env.Volume.VolumeID, env.Volume.Name)
}

// Only the plugin's returned state is persisted and used for Destroy; neither
// that state nor the caller's independent input may appear in success responses.
func TestVolumeHTTP_PrivateDataPersistedButOmittedFromWire(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	fake.privateData = "volumes/vol-pd/"
	volumeID := "vol-pd"
	const input = "caller-private-data-A"

	w, created := doVolumeRequest(t, r, http.MethodPost, "/cube/volume",
		volumeCreateBody(t, volumeID, fake.Name(), input))
	require.Equal(t, 0, created.Ret.RetCode)
	require.NotNil(t, created.Volume)
	assert.Equal(t, input, fake.createInputs[volumeID])
	assertVolumeResponseOmitsPrivateData(t, w, input, fake.privateData)

	var record models.VolumeRecord
	require.NoError(t, db.Where("volume_id = ?", volumeID).First(&record).Error)
	assert.Equal(t, "volumes/vol-pd/", record.PrivateData)
	for _, path := range []string{"/cube/volume/" + volumeID, "/cube/volume"} {
		w, env := doVolumeRequest(t, r, http.MethodGet, path, "")
		require.Zero(t, env.Ret.RetCode)
		assertVolumeResponseOmitsPrivateData(t, w, input, fake.privateData)
	}
	_, deleted := doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+volumeID, "")
	require.Zero(t, deleted.Ret.RetCode)
	assert.Equal(t, fake.privateData, fake.destroyInputs[volumeID])
	assert.Equal(t, 1, fake.createCalls)
	assert.Equal(t, 1, fake.destroyCalls)
}

// Oversized Create private_data must reject and roll back the reserved row.
func TestVolumeHTTP_PrivateDataTooLongRejected(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	fake.privateData = strings.Repeat("a", models.MaxPrivateDataLen+1)
	volumeID := "vol-pd-long"

	w, env := doVolumeRequestWithStatus(t, r, http.MethodPost, "/cube/volume",
		volumeCreateBody(t, volumeID, fake.Name(), "caller-input-A"), http.StatusBadRequest)
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), env.Ret.RetCode)
	assert.Contains(t, env.Ret.RetMsg, "private_data")
	assert.True(t, fake.wasDestroyed(volumeID), "plugin Destroy must run after oversized private_data")
	assert.Equal(t, fake.privateData, fake.destroyInputs[volumeID], "rollback must receive the complete returned state")
	assert.Equal(t, 1, fake.destroyCalls)
	assert.NotContains(t, w.Body.String(), fake.privateData)

	var record models.VolumeRecord
	err := db.Where("volume_id = ?", volumeID).First(&record).Error
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestVolumeHTTP_PrivateDataEmptyPluginResponse(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	const volumeID = "vol-empty-plugin-state"
	const input = "caller-state-must-not-persist"
	_, created := doVolumeRequest(t, r, http.MethodPost, "/cube/volume",
		volumeCreateBody(t, volumeID, fake.Name(), input))
	require.Zero(t, created.Ret.RetCode)
	assert.Equal(t, input, fake.createInputs[volumeID])
	var record models.VolumeRecord
	require.NoError(t, db.Where("volume_id = ?", volumeID).First(&record).Error)
	assert.Empty(t, record.PrivateData, "empty plugin output must not fall back to caller input")
	_, deleted := doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+volumeID, "")
	require.Zero(t, deleted.Ret.RetCode)
	assert.Equal(t, "", fake.destroyInputs[volumeID])
	assert.Equal(t, 1, fake.destroyCalls)
}

func TestVolumeHTTP_PrivateDataDBUpdateFailureUsesReturnedState(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	const volumeID = "vol-update-failure"
	fake.privateData = "plugin-returned-state-B"
	const callbackName = "test:fail_volume_update"
	updateAttempted := false
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == models.VolumeTableName {
			updateAttempted = true
			tx.AddError(errors.New("injected volume update failure"))
		}
	}))
	t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove(callbackName)) })

	_, env := doVolumeRequest(t, r, http.MethodPost, "/cube/volume",
		volumeCreateBody(t, volumeID, fake.Name(), "caller-input-A"))
	assert.True(t, updateAttempted, "the failure must occur after the plugin succeeds")
	assert.Equal(t, int(errorcode.ErrorCode_DBError), env.Ret.RetCode)
	assert.Equal(t, 1, fake.createCalls)
	assert.Equal(t, 1, fake.destroyCalls)
	assert.Equal(t, fake.privateData, fake.destroyInputs[volumeID])
	var record models.VolumeRecord
	assert.ErrorIs(t, db.Where("volume_id = ?", volumeID).First(&record).Error, gorm.ErrRecordNotFound)
}

func TestVolumeHTTP_PrivateDataDestroyErrorPreservesRecord(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	const volumeID = "vol-destroy-failure"
	fake.privateData = "plugin-returned-state-B"
	_, created := doVolumeRequest(t, r, http.MethodPost, "/cube/volume",
		volumeCreateBody(t, volumeID, fake.Name(), "caller-input-A"))
	require.Zero(t, created.Ret.RetCode)
	fake.destroyErr = errors.New("backend temporarily unavailable")
	_, env := doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+volumeID, "")
	assert.Equal(t, int(errorcode.ErrorCode_MasterInternalError), env.Ret.RetCode)
	assert.Equal(t, 1, fake.destroyCalls)
	assert.Equal(t, fake.privateData, fake.destroyInputs[volumeID])
	var record models.VolumeRecord
	require.NoError(t, db.Where("volume_id = ?", volumeID).First(&record).Error)
	assert.Equal(t, fake.privateData, record.PrivateData)
}

func TestVolumeHTTP_ReservationFailureDoesNotCallPlugin(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	const callbackName = "test:fail_volume_reservation"
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == models.VolumeTableName {
			tx.AddError(errors.New("injected volume reservation failure"))
		}
	}))
	t.Cleanup(func() { require.NoError(t, db.Callback().Create().Remove(callbackName)) })
	_, env := doVolumeRequest(t, r, http.MethodPost, "/cube/volume",
		volumeCreateBody(t, "vol-reservation-failure", fake.Name(), "caller-input-A"))
	assert.Equal(t, int(errorcode.ErrorCode_DBError), env.Ret.RetCode)
	assert.Zero(t, fake.createCalls)
	assert.Zero(t, fake.destroyCalls)
	var count int64
	require.NoError(t, db.Model(&models.VolumeRecord{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestIsValidVolumeName(t *testing.T) {
	assert.True(t, isValidVolumeName("vol-ok_1"))
	assert.False(t, isValidVolumeName(""))
	assert.False(t, isValidVolumeName("has space"))
	assert.False(t, isValidVolumeName("bad/name"))
}

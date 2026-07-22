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
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// fakeControllerPlugin is an in-process ControllerPlugin used by volume HTTP
// tests. It does not touch COS or any external backend.
type fakeControllerPlugin struct {
	name string

	// createErr, when set, makes Create fail so tests can exercise the
	// handler's DB-row rollback path.
	createErr error
	// privateData is returned from Create when non-empty.
	privateData string

	mu        sync.Mutex
	created   map[string]string // volumeID → name
	destroyed map[string]bool
}

func newFakeControllerPlugin(name string) *fakeControllerPlugin {
	return &fakeControllerPlugin{
		name:      name,
		created:   map[string]string{},
		destroyed: map[string]bool{},
	}
}

func (f *fakeControllerPlugin) Name() string { return f.name }

func (f *fakeControllerPlugin) Create(_ context.Context, volumeID, name string) (*plugin.VolumeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

func (f *fakeControllerPlugin) Destroy(_ context.Context, volumeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyed[volumeID] = true
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
	t.Cleanup(func() { volumeDB = origDB })

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
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "CubeMaster volume handlers always use HTTP 200 + ret envelope; body=%s", w.Body.String())

	var env volumeRetEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), "body=%s", w.Body.String())
	require.NotNil(t, env.Ret, "response must carry ret envelope: %s", w.Body.String())
	return w, env
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

	// After refcount drops, delete succeeds.
	require.NoError(t, db.Model(&models.VolumeRecord{}).
		Where("volume_id = ?", volumeID).
		Update("refcount", 0).Error)
	_, env = doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+volumeID, "")
	assert.Equal(t, 0, env.Ret.RetCode)
	assert.True(t, fake.wasDestroyed(volumeID))
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
		fmt.Sprintf(`{"name":%q,"driver":%q}`, volumeID, fake.Name()))
	assert.Equal(t, int(errorcode.ErrorCode_MasterInternalError), env.Ret.RetCode)

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

// Create private_data is persisted in DB but never returned on the wire VolumeItem.
func TestVolumeHTTP_PrivateDataPersistedButOmittedFromWire(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	fake.privateData = "volumes/vol-pd/"
	volumeID := "vol-pd"

	_, created := doVolumeRequest(t, r, http.MethodPost, "/cube/volume",
		fmt.Sprintf(`{"name":%q,"driver":%q}`, volumeID, fake.Name()))
	require.Equal(t, 0, created.Ret.RetCode)
	require.NotNil(t, created.Volume)

	raw, err := json.Marshal(created.Volume)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	_, hasSnake := m["private_data"]
	_, hasCamel := m["privateData"]
	assert.False(t, hasSnake || hasCamel, "wire VolumeItem must omit private_data: %s", raw)

	var record models.VolumeRecord
	require.NoError(t, db.Where("volume_id = ?", volumeID).First(&record).Error)
	assert.Equal(t, "volumes/vol-pd/", record.PrivateData)
}

// Oversized Create private_data must reject and roll back the reserved row.
func TestVolumeHTTP_PrivateDataTooLongRejected(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	fake.privateData = strings.Repeat("a", models.MaxPrivateDataLen+1)
	volumeID := "vol-pd-long"

	_, env := doVolumeRequest(t, r, http.MethodPost, "/cube/volume",
		fmt.Sprintf(`{"name":%q,"driver":%q}`, volumeID, fake.Name()))
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), env.Ret.RetCode)
	assert.Contains(t, env.Ret.RetMsg, "private_data")
	assert.True(t, fake.wasDestroyed(volumeID), "plugin Destroy must run after oversized private_data")

	var record models.VolumeRecord
	err := db.Where("volume_id = ?", volumeID).First(&record).Error
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestIsValidVolumeName(t *testing.T) {
	assert.True(t, isValidVolumeName("vol-ok_1"))
	assert.False(t, isValidVolumeName(""))
	assert.False(t, isValidVolumeName("has space"))
	assert.False(t, isValidVolumeName("bad/name"))
}

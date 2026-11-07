// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"gorm.io/gorm"
)

func refCountOf(t *testing.T, db *gorm.DB, volumeID string) int64 {
	t.Helper()
	var rec models.VolumeRecord
	require.NoError(t, db.Where("volume_id = ?", volumeID).First(&rec).Error)
	return rec.RefCount
}

func createTestVolume(t *testing.T, r *gin.Engine, driver, volumeID string) {
	t.Helper()
	_, env := doVolumeRequest(t, r, http.MethodPost, "/cube/volume",
		fmt.Sprintf(`{"name":%q,"driver":%q}`, volumeID, driver))
	require.Equal(t, 0, env.Ret.RetCode)
}

// Direct content access takes a reference with POST .../ref and gives it back
// with DELETE .../ref; the volume cannot be deleted in between.
func TestVolumeRef_BlocksDeleteWhileHeld(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	const id = "vol-ref-1"
	createTestVolume(t, r, fake.Name(), id)

	_, env := doVolumeRequest(t, r, http.MethodPost, "/cube/volume/"+id+"/ref", "")
	require.Equal(t, 0, env.Ret.RetCode)
	assert.Equal(t, id, env.VolumeID)
	assert.EqualValues(t, 1, refCountOf(t, db, id))

	_, env = doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+id, "")
	assert.Equal(t, int(errorcode.ErrorCode_Conflict), env.Ret.RetCode)
	assert.Contains(t, env.Ret.RetMsg, "in use")
	assert.False(t, fake.wasDestroyed(id), "backend must not be destroyed while referenced")

	_, env = doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+id+"/ref", "")
	require.Equal(t, 0, env.Ret.RetCode)
	assert.EqualValues(t, 0, refCountOf(t, db, id))

	_, env = doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+id, "")
	assert.Equal(t, 0, env.Ret.RetCode)
	assert.True(t, fake.wasDestroyed(id))
}

// A reference may not be taken on a volume that does not exist (never created,
// or already deleted); this is how Cubelet learns not to attach it.
func TestVolumeRef_AcquireOnMissingVolumeIsNotFound(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)

	_, env := doVolumeRequest(t, r, http.MethodPost, "/cube/volume/never-created/ref", "")
	assert.Equal(t, int(errorcode.ErrorCode_NotFound), env.Ret.RetCode)

	const id = "vol-ref-gone"
	createTestVolume(t, r, fake.Name(), id)
	_, env = doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+id, "")
	require.Equal(t, 0, env.Ret.RetCode)

	_, env = doVolumeRequest(t, r, http.MethodPost, "/cube/volume/"+id+"/ref", "")
	assert.Equal(t, int(errorcode.ErrorCode_NotFound), env.Ret.RetCode)

	var n int64
	require.NoError(t, db.Model(&models.VolumeRecord{}).Count(&n).Error)
	assert.EqualValues(t, 0, n, "acquire must not create or resurrect a row")
}

// Release is idempotent: it never drives the count negative and succeeds for a
// volume that is gone, so a late or repeated release is harmless.
func TestVolumeRef_ReleaseIsClampedAndTolerant(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	const id = "vol-ref-clamp"
	createTestVolume(t, r, fake.Name(), id)

	for i := 0; i < 3; i++ {
		_, env := doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+id+"/ref", "")
		assert.Equal(t, 0, env.Ret.RetCode)
	}
	assert.EqualValues(t, 0, refCountOf(t, db, id), "count must stay at zero")

	_, env := doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/does-not-exist/ref", "")
	assert.Equal(t, 0, env.Ret.RetCode)
}

// Sandbox references (Cubelet ext_info events) and direct-access references are
// the same counter, so each independently keeps the volume from being deleted.
func TestVolumeRef_SharesCounterWithSandboxes(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	const id = "vol-ref-shared"
	createTestVolume(t, r, fake.Name(), id)

	// A sandbox on node A references the volume (what ApplyFromExtInfo does).
	require.NoError(t, db.Model(&models.VolumeRecord{}).
		Where("volume_id = ?", id).Update("refcount", gorm.Expr("refcount + 1")).Error)
	_, env := doVolumeRequest(t, r, http.MethodPost, "/cube/volume/"+id+"/ref", "")
	require.Equal(t, 0, env.Ret.RetCode)
	assert.EqualValues(t, 2, refCountOf(t, db, id))

	// Direct access finishes first: the sandbox still blocks deletion.
	_, env = doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+id+"/ref", "")
	require.Equal(t, 0, env.Ret.RetCode)
	_, env = doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+id, "")
	assert.Equal(t, int(errorcode.ErrorCode_Conflict), env.Ret.RetCode)
	assert.False(t, fake.wasDestroyed(id))

	// Sandbox goes away: now it can be deleted.
	require.NoError(t, db.Model(&models.VolumeRecord{}).
		Where("volume_id = ?", id).Update("refcount", gorm.Expr("refcount - 1")).Error)
	_, env = doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+id, "")
	assert.Equal(t, 0, env.Ret.RetCode)
	assert.True(t, fake.wasDestroyed(id))
}

// References taken concurrently are all counted and all returned, and a delete
// racing with them either wins before any reference (every acquire then reports
// NotFound) or loses with Conflict; it never destroys a referenced volume.
func TestVolumeRef_ConcurrentAcquireReleaseAndDelete(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	const id = "vol-ref-race"
	createTestVolume(t, r, fake.Name(), id)

	const holders = 8
	var wg sync.WaitGroup
	acquired := make([]bool, holders)
	for i := 0; i < holders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, env := doVolumeRequest(t, r, http.MethodPost, "/cube/volume/"+id+"/ref", "")
			acquired[i] = env.Ret.RetCode == 0
		}(i)
	}
	wg.Wait()
	held := 0
	for _, ok := range acquired {
		if ok {
			held++
		}
	}
	require.Equal(t, holders, held, "no delete was issued, so every acquire must succeed")
	assert.EqualValues(t, holders, refCountOf(t, db, id))

	_, env := doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+id, "")
	assert.Equal(t, int(errorcode.ErrorCode_Conflict), env.Ret.RetCode)

	for i := 0; i < holders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+id+"/ref", "")
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 0, refCountOf(t, db, id))

	_, env = doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+id, "")
	assert.Equal(t, 0, env.Ret.RetCode)
	assert.True(t, fake.wasDestroyed(id))
}

// A failed plugin Destroy must leave the volume row intact and usable.
func TestVolumeDelete_DestroyFailureKeepsRow(t *testing.T) {
	r, db, fake := newVolumeTestEngine(t)
	const id = "vol-destroy-fail"
	createTestVolume(t, r, fake.Name(), id)
	fake.destroyErr = fmt.Errorf("backend unavailable")

	_, env := doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/"+id, "")
	assert.Equal(t, int(errorcode.ErrorCode_MasterInternalError), env.Ret.RetCode)
	assert.EqualValues(t, 0, refCountOf(t, db, id), "row must still exist")

	_, env = doVolumeRequest(t, r, http.MethodPost, "/cube/volume/"+id+"/ref", "")
	assert.Equal(t, 0, env.Ret.RetCode, "volume must still be usable after a failed delete")
}

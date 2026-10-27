// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//
//

package cubebox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

func admissionServiceForTest(t *testing.T, resuming *cubeboxstore.CubeBox, list []*cubeboxstore.CubeBox,
	ratio float64, cpu int, mem string) *service {
	t.Helper()
	_, err := config.Init("", true)
	require.NoError(t, err)
	hostConf := config.GetHostConf()
	hostConf.Quota.PausedResourceReleaseRatio = ratio
	hostConf.Quota.Cpu = cpu
	hostConf.Quota.Mem = mem
	t.Cleanup(func() {
		hostConf.Quota.PausedResourceReleaseRatio = 0
		hostConf.Quota.Cpu = 0
		hostConf.Quota.Mem = ""
	})
	return &service{cubeboxMgr: &local{cubeboxManger: &fakeCubeboxAPI{cb: resuming, list: list}}}
}

func reproNodeForTest() (paused1, paused2 *cubeboxstore.CubeBox, list []*cubeboxstore.CubeBox) {
	now := time.Now().UnixNano()
	run1 := sandboxWithResourceForTest("run-1", cubeboxstore.Status{StartedAt: now}, "1000m", "2Gi", 1, 0, 0)
	run2 := sandboxWithResourceForTest("run-2", cubeboxstore.Status{StartedAt: now}, "1000m", "2Gi", 1, 0, 0)
	run3 := sandboxWithResourceForTest("run-3", cubeboxstore.Status{StartedAt: now}, "1000m", "2Gi", 1, 0, 0)
	paused1 = sandboxWithResourceForTest("paused-1", cubeboxstore.Status{PausedAt: now}, "1000m", "2Gi", 1, 0, 0)
	paused2 = sandboxWithResourceForTest("paused-2", cubeboxstore.Status{PausedAt: now}, "1000m", "2Gi", 1, 0, 0)
	for _, sb := range []*cubeboxstore.CubeBox{run1, run2, run3, paused1, paused2} {
		sb.SandboxID = sb.ID
	}
	markBackendForTest("s3", run1, run2, run3, paused1, paused2)
	return paused1, paused2, []*cubeboxstore.CubeBox{run1, run2, run3, paused1, paused2}
}

type storeViewCubeboxAPI struct {
	fakeCubeboxAPI
	mu     sync.Mutex
	byID   map[string]*cubeboxstore.CubeBox
	list   []*cubeboxstore.CubeBox
	getErr error
}

func (f *storeViewCubeboxAPI) Get(_ context.Context, id string) (*cubeboxstore.CubeBox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.byID[id], nil
}

func (f *storeViewCubeboxAPI) List() []*cubeboxstore.CubeBox {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list
}

func (f *storeViewCubeboxAPI) replaceRow(id string, status cubeboxstore.Status, sb *cubeboxstore.CubeBox) {
	f.mu.Lock()
	defer f.mu.Unlock()
	next := sandboxWithResourceForTest(id, status, "1000m", sb.ResourceWithOverHead.HostMemQ.String(), 1, 0, 0)
	next.SandboxID = id
	f.byID[id] = next
	for i, cur := range f.list {
		if cur != nil && cur.ID == id {
			f.list[i] = next
		}
	}
}

func (f *storeViewCubeboxAPI) dropRow(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.byID, id)
	kept := f.list[:0]
	for _, cur := range f.list {
		if cur != nil && cur.ID == id {
			continue
		}
		kept = append(kept, cur)
	}
	f.list = kept
}

func TestAdmitAndReserveResumeNoOpWhenPolicyDisabled(t *testing.T) {
	paused1, _, list := reproNodeForTest()
	s := admissionServiceForTest(t, paused1, list, 0, 4000, "8Gi")

	release, rejected := s.cubeboxMgr.admitAndReserveResume(context.Background(), paused1.ID)

	require.Nil(t, rejected)
	require.NotNil(t, release)
	assert.Empty(t, s.cubeboxMgr.resumeReserved, "policy off reserves nothing")
	release()
}

func TestAdmitAndReserveResumeAdmitsWhenNoLocalRecord(t *testing.T) {
	_, _, list := reproNodeForTest()
	s := admissionServiceForTest(t, nil, list, 1.0, 4000, "8Gi")

	release, rejected := s.cubeboxMgr.admitAndReserveResume(context.Background(), "sb-elsewhere")

	require.Nil(t, rejected)
	assert.Empty(t, s.cubeboxMgr.resumeReserved)
	release()
}

func TestAdmitAndReserveResumeRejectsWhenResourceMetadataMissing(t *testing.T) {
	sb := newCubeboxWithStatusForTest("sb-no-res", cubeboxstore.Status{PausedAt: time.Now().UnixNano()})
	sb.SandboxID = sb.ID
	s := admissionServiceForTest(t, sb, []*cubeboxstore.CubeBox{sb}, 1.0, 4000, "4Gi")

	release, rejected := s.cubeboxMgr.admitAndReserveResume(context.Background(), sb.ID)

	require.NotNil(t, rejected)
	assert.Equal(t, errorcode.ErrorCode_Conflict, rejected.RetCode)
	assert.Contains(t, rejected.RetMsg, "missing resource metadata")
	assert.Empty(t, s.cubeboxMgr.resumeReserved)
	release()
}

func TestAdmitAndReserveResumeAdmitsAndReleases(t *testing.T) {
	paused1, _, list := reproNodeForTest()
	s := admissionServiceForTest(t, paused1, list, 1.0, 8000, "8Gi")

	release, rejected := s.cubeboxMgr.admitAndReserveResume(context.Background(), paused1.ID)
	require.Nil(t, rejected, "6144MB used + 2048MB resume == 8192MB quota must fit")

	s.cubeboxMgr.resumeAdmitMu.Lock()
	reserved := s.cubeboxMgr.resumeReserved[paused1.ID]
	s.cubeboxMgr.resumeAdmitMu.Unlock()
	twoGi := resource.MustParse("2Gi")
	assert.Equal(t, twoGi.Value(), reserved.memBytes,
		"the reservation carries the sandbox's full demand; only the part the store row is not already charging is added")
	assert.Equal(t, int64(1000), reserved.cpuMilli)

	release()

	s.cubeboxMgr.resumeAdmitMu.Lock()
	_, stillThere := s.cubeboxMgr.resumeReserved[paused1.ID]
	s.cubeboxMgr.resumeAdmitMu.Unlock()
	assert.False(t, stillThere, "release must drop the reservation")
}

func TestAdmitAndReserveResumeRejectsSecondConcurrentResume(t *testing.T) {
	paused1, paused2, list := reproNodeForTest()
	s := admissionServiceForTest(t, paused1, list, 1.0, 8000, "8Gi")

	release1, rejected1 := s.cubeboxMgr.admitAndReserveResume(context.Background(), paused1.ID)
	require.Nil(t, rejected1, "first resume fits exactly (6144+2048==8192)")

	fake := s.cubeboxMgr.cubeboxManger.(*fakeCubeboxAPI)
	fake.cb = paused2
	_, rejected2 := s.cubeboxMgr.admitAndReserveResume(context.Background(), paused2.ID)

	require.NotNil(t, rejected2, "the node cannot hold a fifth running 2Gi sandbox")
	assert.Equal(t, errorcode.ErrorCode_Conflict, rejected2.RetCode)
	assert.Contains(t, rejected2.RetMsg, "mem quota")

	release1()
	_, rejected2b := s.cubeboxMgr.admitAndReserveResume(context.Background(), paused2.ID)
	assert.Nil(t, rejected2b, "with the first reservation gone the node fits one resume again")
}

func TestAdmitAndReserveResumeReentrantSameSandboxNotChargedTwice(t *testing.T) {
	paused1, _, list := reproNodeForTest()
	s := admissionServiceForTest(t, paused1, list, 1.0, 8000, "8Gi")

	_, rejected1 := s.cubeboxMgr.admitAndReserveResume(context.Background(), paused1.ID)
	require.Nil(t, rejected1)

	_, rejected2 := s.cubeboxMgr.admitAndReserveResume(context.Background(), paused1.ID)
	assert.Nil(t, rejected2, "re-admitting the same sandbox must not be charged against itself")
}

func TestAdmitAndReserveResumeConcurrentAdmitsExactlyOne(t *testing.T) {
	paused1, paused2, list := reproNodeForTest()
	s := admissionServiceForTest(t, paused1, list, 1.0, 8000, "8Gi")
	// Both ids must resolve, so back Get with a list-aware lookup rather than
	// the single-cb fake.
	s.cubeboxMgr.cubeboxManger = &listBackedCubeboxAPI{byID: map[string]*cubeboxstore.CubeBox{
		paused1.ID: paused1,
		paused2.ID: paused2,
	}, list: list}

	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for _, id := range []string{paused1.ID, paused2.ID} {
		wg.Add(1)
		go func(sandboxID string) {
			defer wg.Done()
			_, rejected := s.cubeboxMgr.admitAndReserveResume(context.Background(), sandboxID)
			if rejected == nil {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()

	assert.Equal(t, 1, admitted, "exactly one of two concurrent resumes may be admitted onto a one-slot node")
}

func TestAdmitAndReserveResumeSkipsRecordThatIsNotPaused(t *testing.T) {
	now := time.Now().UnixNano()
	running := sandboxWithResourceForTest("sb-running", cubeboxstore.Status{StartedAt: now}, "1000m", "2Gi", 1, 0, 0)
	running.SandboxID = running.ID
	// Quota so tight that charging this sandbox at all would reject it.
	s := admissionServiceForTest(t, running, []*cubeboxstore.CubeBox{running}, 1.0, 4000, "2Gi")

	release, rejected := s.cubeboxMgr.admitAndReserveResume(context.Background(), running.ID)

	require.Nil(t, rejected, "a non-PAUSED record must not be admitted or charged here")
	assert.Empty(t, s.cubeboxMgr.resumeReserved)
	release()
}

func TestAdmitAndReserveResumeFailsClosedOnStoreReadError(t *testing.T) {
	paused1, _, list := reproNodeForTest()
	s := admissionServiceForTest(t, paused1, list, 1.0, 8000, "8Gi")
	s.cubeboxMgr.cubeboxManger = &storeViewCubeboxAPI{
		byID:   map[string]*cubeboxstore.CubeBox{paused1.ID: paused1},
		list:   list,
		getErr: errors.New("bolt: database not open"),
	}

	_, rejected := s.cubeboxMgr.admitAndReserveResume(context.Background(), paused1.ID)

	require.NotNil(t, rejected)
	assert.Equal(t, errorcode.ErrorCode_Conflict, rejected.RetCode)
	assert.Contains(t, rejected.RetMsg, "cannot read sandbox state")
	assert.Empty(t, s.cubeboxMgr.resumeReserved)
}

func TestAdmitAndReserveResumeAdmitsWhenLocalRecordIsNotFound(t *testing.T) {
	_, _, list := reproNodeForTest()
	s := admissionServiceForTest(t, nil, list, 1.0, 8000, "8Gi")
	s.cubeboxMgr.cubeboxManger = &storeViewCubeboxAPI{
		byID:   map[string]*cubeboxstore.CubeBox{},
		list:   list,
		getErr: utils.ErrorKeyNotFound,
	}

	release, rejected := s.cubeboxMgr.admitAndReserveResume(context.Background(), "sb-elsewhere")

	require.Nil(t, rejected, "no local record means this node is not the one that paused it")
	assert.Empty(t, s.cubeboxMgr.resumeReserved)
	release()
}

func TestAdmitAndReserveResumeReservationStopsWhenRowIsRepersisted(t *testing.T) {
	now := time.Now().UnixNano()
	run1 := sandboxWithResourceForTest("run-1", cubeboxstore.Status{StartedAt: now}, "1000m", "2Gi", 1, 0, 0)
	run2 := sandboxWithResourceForTest("run-2", cubeboxstore.Status{StartedAt: now}, "1000m", "2Gi", 1, 0, 0)
	paused1 := sandboxWithResourceForTest("paused-1", cubeboxstore.Status{PausedAt: now}, "1000m", "2Gi", 1, 0, 0)
	paused2 := sandboxWithResourceForTest("paused-2", cubeboxstore.Status{PausedAt: now}, "1000m", "2Gi", 1, 0, 0)
	list := []*cubeboxstore.CubeBox{run1, run2, paused1, paused2}
	for _, sb := range list {
		sb.SandboxID = sb.ID
	}
	markBackendForTest("s3", list...)
	store := &storeViewCubeboxAPI{
		byID: map[string]*cubeboxstore.CubeBox{paused1.ID: paused1, paused2.ID: paused2},
		list: list,
	}
	s := admissionServiceForTest(t, paused1, list, 1.0, 8000, "8Gi")
	s.cubeboxMgr.cubeboxManger = store

	_, rejected1 := s.cubeboxMgr.admitAndReserveResume(context.Background(), paused1.ID)
	require.Nil(t, rejected1, "4096MB used + 2048MB resume fits in 8192MB")

	// The create flow saves the new row: paused-1 is now charged in full.
	store.replaceRow(paused1.ID, cubeboxstore.Status{CreatedAt: now}, paused1)

	_, rejected2 := s.cubeboxMgr.admitAndReserveResume(context.Background(), paused2.ID)
	require.Nil(t, rejected2,
		"6144MB charged by the store + 2048MB for paused-2 == 8192MB: this fits, so it must not be rejected")
}

func TestAdmitAndReserveResumeReservationCoversTheDeletedRowWindow(t *testing.T) {
	now := time.Now().UnixNano()
	run1 := sandboxWithResourceForTest("run-1", cubeboxstore.Status{StartedAt: now}, "1000m", "2560Mi", 1, 0, 0)
	run2 := sandboxWithResourceForTest("run-2", cubeboxstore.Status{StartedAt: now}, "1000m", "2560Mi", 1, 0, 0)
	paused1 := sandboxWithResourceForTest("paused-1", cubeboxstore.Status{PausedAt: now}, "1000m", "2Gi", 1, 0, 0)
	paused2 := sandboxWithResourceForTest("paused-2", cubeboxstore.Status{PausedAt: now}, "1000m", "2Gi", 1, 0, 0)
	list := []*cubeboxstore.CubeBox{run1, run2, paused1, paused2}
	for _, sb := range list {
		sb.SandboxID = sb.ID
	}
	markBackendForTest("s3", list...)
	store := &storeViewCubeboxAPI{
		byID: map[string]*cubeboxstore.CubeBox{paused1.ID: paused1, paused2.ID: paused2},
		list: list,
	}
	s := admissionServiceForTest(t, paused1, list, 0.5, 8000, "8Gi")
	s.cubeboxMgr.cubeboxManger = store

	_, rejected1 := s.cubeboxMgr.admitAndReserveResume(context.Background(), paused1.ID)
	require.Nil(t, rejected1, "5120 + 2x1024 (paused at half) + 1024 (half of paused-1) == 8192")

	// The create flow deletes the PAUSED tombstone before it saves the new row.
	store.dropRow(paused1.ID)

	_, rejected2 := s.cubeboxMgr.admitAndReserveResume(context.Background(), paused2.ID)
	require.NotNil(t, rejected2,
		"paused-1 is committed but charged nowhere: resuming paused-2 too would need 5120+2048+2048 > 8192")
	assert.Equal(t, errorcode.ErrorCode_Conflict, rejected2.RetCode)
}

func TestAdmitAndReserveResumeSkipsSnapshotThatCannotLeaveTheNode(t *testing.T) {
	paused1, _, list := reproNodeForTest()
	markBackendForTest("xfs", paused1)
	s := admissionServiceForTest(t, paused1, list, 1.0, 4000, "4Gi")

	release, rejected := s.cubeboxMgr.admitAndReserveResume(context.Background(), paused1.ID)

	require.Nil(t, rejected, "an XFS snapshot keeps its quota, so its resume fits by construction")
	assert.Empty(t, s.cubeboxMgr.resumeReserved)
	release()
}

func TestResumeAdmissionNeedMatchesWhatAccountingReleased(t *testing.T) {
	now := time.Now().UnixNano()
	paused := sandboxWithResourceForTest("paused", cubeboxstore.Status{PausedAt: now}, "1000m", "2Gi", 1, 0, 0)
	paused.SandboxID = paused.ID

	for _, tc := range []struct {
		name    string
		backend string
		ratio   float64
		wantOff bool
	}{
		{"s3 releases half", "s3", 0.5, false},
		{"s3 releases all", "s3", 1.0, false},
		{"xfs releases nothing", "xfs", 1.0, true},
		{"no backend releases nothing", "", 1.0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			markBackendForTest(tc.backend, paused)
			_, err := config.Init("", true)
			require.NoError(t, err)
			hostConf := config.GetHostConf()
			hostConf.Quota.PausedResourceReleaseRatio = tc.ratio
			defer func() { hostConf.Quota.PausedResourceReleaseRatio = 0 }()

			in, state := resumeAdmissionInputs(paused)
			if tc.wantOff {
				assert.Equal(t, resumeAdmissionOff, state, "nothing was released, so no check at all")
				return
			}
			require.Equal(t, resumeAdmissionCheck, state)
			alloc := aggregateSandboxResources([]*cubeboxstore.CubeBox{paused}, tc.ratio)
			assert.Equal(t, int64(2048), alloc.MemoryMB+in.needMemMB,
				"what the row still charges + what admission asks for == full quota")
			assert.Equal(t, int64(1000), alloc.MilliCPU+in.needCPUMilli)
		})
	}
}

type listBackedCubeboxAPI struct {
	fakeCubeboxAPI
	byID map[string]*cubeboxstore.CubeBox
	list []*cubeboxstore.CubeBox
}

func (f *listBackedCubeboxAPI) Get(_ context.Context, id string) (*cubeboxstore.CubeBox, error) {
	return f.byID[id], nil
}

func (f *listBackedCubeboxAPI) List() []*cubeboxstore.CubeBox { return f.list }

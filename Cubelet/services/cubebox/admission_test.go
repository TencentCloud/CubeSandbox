// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type admissionStore struct {
	cubes.CubeboxAPI
	boxes   []*cubeboxstore.CubeBox
	saveErr error
}

func (s *admissionStore) List() []*cubeboxstore.CubeBox { return s.boxes }
func (s *admissionStore) Save(_ context.Context, sb *cubeboxstore.CubeBox, _ ...cubes.UpdateCubeboxOpt) error {
	// The production store publishes in memory before synchronizing to disk.
	s.boxes = append(s.boxes, sb)
	return s.saveErr
}

func admissionBox(id, cpu, mem string) *cubeboxstore.CubeBox {
	return sandboxWithResourceForTest(id, cubeboxstore.Status{CreatedAt: time.Now().UnixNano()}, cpu, mem, 0, 0, 0)
}

func TestCreateAdmissionConcurrentLastSlot(t *testing.T) {
	for _, count := range []int{10, 100, 1000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			store := &admissionStore{boxes: []*cubeboxstore.CubeBox{admissionBox("existing", "63", "63Gi")}}
			l := &local{cubeboxManger: store}
			var accepted, rejected atomic.Int64
			var wg sync.WaitGroup
			for i := 0; i < count; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					err := l.admitAndSaveWithQuota(context.Background(), admissionBox(fmt.Sprint(i), "1", "1Gi"), &config.HostConf{}, 64000, 65536)
					if err == nil {
						accepted.Add(1)
						return
					}
					status, ok := ret.FromError(err)
					if ok && status.Code() == errorcode.ErrorCode_HostResourcesNotEnough {
						rejected.Add(1)
					}
				}(i)
			}
			wg.Wait()
			require.Equal(t, int64(1), accepted.Load())
			require.Equal(t, int64(count-1), rejected.Load())
			require.Len(t, store.boxes, 2)
			// Ordinary deletion releases the reservation without another ledger.
			store.boxes = store.boxes[:1]
			require.NoError(t, l.admitAndSaveWithQuota(context.Background(), admissionBox("replacement", "1", "1Gi"), &config.HostConf{}, 64000, 65536))
		})
	}
}

func TestCreateAdmissionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, cpu, mem string
		denied         bool
	}{
		{"cpu below", "999m", "1Gi", false}, {"cpu exact", "1", "1Gi", false}, {"cpu above", "1001m", "1Gi", true},
		{"memory below", "1", "1073741823", false}, {"memory exact", "1", "1Gi", false}, {"memory above", "1", "1073741825", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCreateQuota(nil, admissionBox("new", tc.cpu, tc.mem), &config.HostConf{}, 1000, 1024)
			require.Equal(t, tc.denied, err != nil)
		})
	}
	box := admissionBox("old", "1", "1073741823")
	require.Error(t, checkCreateQuota([]*cubeboxstore.CubeBox{box}, admissionBox("new", "1", "2"), &config.HostConf{}, 2000, 1024))
	box.ResourceWithOverHead = nil
	require.ErrorContains(t, checkCreateQuota([]*cubeboxstore.CubeBox{box}, admissionBox("new", "1", "1Gi"), &config.HostConf{}, 2000, 2048), "no host resource")
}

func TestRestartAdmissionReplacesOnlyItsRetainedAllocation(t *testing.T) {
	for _, withOther := range []bool{false, true} {
		store := &admissionStore{boxes: []*cubeboxstore.CubeBox{admissionBox("same", "1", "1Gi")}}
		if withOther {
			store.boxes = append(store.boxes, admissionBox("other", "1", "1Gi"))
		}
		l := &local{cubeboxManger: store}
		require.Error(t, l.admitAndSaveWithQuota(context.Background(), admissionBox("same", "1", "1Gi"), &config.HostConf{}, 1000, 1024))
		err := l.admitAndSaveWithQuota(context.Background(), admissionBox("same", "1", "1Gi"), &config.HostConf{}, 1000, 1024, true)
		if withOther {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
	}
}

func TestCreateAdmissionCountsPausedAndInstanceLimit(t *testing.T) {
	old := admissionBox("old", "1", "1Gi")
	old.GetStatus().Update(func(s cubeboxstore.Status) (cubeboxstore.Status, error) {
		s.PausedAt = time.Now().UnixNano()
		return s, nil
	})
	host := &config.HostConf{}
	require.Error(t, checkCreateQuota([]*cubeboxstore.CubeBox{old}, admissionBox("new", "1", "1Gi"), host, 1000, 1024))
	host.Quota.PausedResourceReleaseRatio = 1
	require.NoError(t, checkCreateQuota([]*cubeboxstore.CubeBox{old}, admissionBox("new", "1", "1Gi"), host, 1000, 1024))
	host.Quota.MvmLimit = 1
	require.Error(t, checkCreateQuota([]*cubeboxstore.CubeBox{old}, admissionBox("new", "1", "1Gi"), host, 1000, 1024))
}

func TestCreateAdmissionSaveFailureRemainsReservedUntilCleanup(t *testing.T) {
	store := &admissionStore{saveErr: errors.New("disk sync failed")}
	l := &local{cubeboxManger: store}
	err := l.admitAndSaveWithQuota(context.Background(), admissionBox("first", "1", "1Gi"), &config.HostConf{}, 1000, 1024)
	status, _ := ret.FromError(err)
	require.Equal(t, errorcode.ErrorCode_UpdateLocalMetaDataFailed, status.Code())
	err = l.admitAndSaveWithQuota(context.Background(), admissionBox("second", "1", "1Gi"), &config.HostConf{}, 1000, 1024)
	status, _ = ret.FromError(err)
	require.Equal(t, errorcode.ErrorCode_HostResourcesNotEnough, status.Code())
	require.Len(t, store.boxes, 1)
}

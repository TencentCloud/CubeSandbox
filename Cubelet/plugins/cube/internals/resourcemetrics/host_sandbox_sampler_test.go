package resourcemetrics

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup/handle"
)

type fakeHostSandboxReader struct {
	mu      sync.Mutex
	usage   handle.UsageSnapshot
	err     error
	groups  []string
	started chan struct{}
	release <-chan struct{}
}

func (r *fakeHostSandboxReader) UsageSnapshot(_ context.Context, group string) (handle.UsageSnapshot, error) {
	r.mu.Lock()
	r.groups = append(r.groups, group)
	usage, err := r.usage, r.err
	r.mu.Unlock()
	if r.started != nil {
		r.started <- struct{}{}
	}
	if r.release != nil {
		<-r.release
	}
	return usage, err
}

func (r *fakeHostSandboxReader) requestedGroups() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.groups...)
}

func TestHostSandboxSamplerEstablishesCompatibilityBaselineForExistingSandbox(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	cb.CGroupPath = "/cube_sandbox/sandbox/7"
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeHostSandboxReader{usage: handle.UsageSnapshot{
		CPUUsageTotalNS:          4_800_000_000_000,
		CPUUserTotalNS:           3_500_000_000_000,
		CPUSystemTotalNS:         1_300_000_000_000,
		CPUThrottledTotalNS:      90,
		CPUPeriodsTotal:          80,
		CPUThrottledPeriodsTotal: 20,
		MemoryCurrentBytes:       4096,
		MemoryLimit:              handle.ResourceLimit{Value: 16384},
		MemoryFailuresTotal:      7,
	}}
	sampler := testHostSandboxSampler(t, store, reader)
	sampler.now = func() time.Time { return time.Unix(10, 0) }

	sampleHostDirect(sampler, cb)
	require.Equal(t, []string{cb.CGroupPath}, reader.requestedGroups())
	require.Equal(t, 1, store.syncs)
	require.Equal(t, &cubeboxstore.HostMetricsBaseline{
		CGroupPath:               cb.CGroupPath,
		CPUUsageTotalNS:          4_800_000_000_000,
		CPUUserTotalNS:           3_500_000_000_000,
		CPUSystemTotalNS:         1_300_000_000_000,
		CPUThrottledTotalNS:      90,
		CPUPeriodsTotal:          80,
		CPUThrottledPeriodsTotal: 20,
		MemoryFailuresTotal:      7,
	}, cb.HostMetricsBaselineCopy())
	latest, ok := sampler.Latest(cb.ID, time.Unix(11, 0))
	require.True(t, ok)
	require.Equal(t, HostSandboxAvailable, latest.Availability)
	require.Equal(t, time.Unix(10, 0).UTC(), latest.CollectedAt)
	require.Zero(t, latest.Snapshot.CPUUsageTotalNS)
	require.Zero(t, latest.Snapshot.MemoryFailuresTotal)
	require.Equal(t, uint64(4096), latest.Snapshot.MemoryCurrentBytes)
	require.Equal(t, uint64(16384), latest.Snapshot.MemoryLimitBytes)

	reader.mu.Lock()
	reader.usage.CPUUsageTotalNS += 75
	reader.usage.CPUUserTotalNS += 40
	reader.usage.CPUSystemTotalNS += 35
	reader.usage.CPUThrottledTotalNS += 9
	reader.usage.CPUPeriodsTotal += 3
	reader.usage.CPUThrottledPeriodsTotal += 2
	reader.usage.MemoryCurrentBytes = 6144
	reader.usage.MemoryFailuresTotal += 2
	reader.mu.Unlock()
	sampler.now = func() time.Time { return time.Unix(15, 0) }
	sampleHostDirect(sampler, cb)
	latest, ok = sampler.Latest(cb.ID, time.Unix(16, 0))
	require.True(t, ok)
	require.Equal(t, uint64(75), latest.Snapshot.CPUUsageTotalNS)
	require.Equal(t, uint64(40), latest.Snapshot.CPUUserTotalNS)
	require.Equal(t, uint64(35), latest.Snapshot.CPUSystemTotalNS)
	require.Equal(t, uint64(9), latest.Snapshot.CPUThrottledTotalNS)
	require.Equal(t, uint64(3), latest.Snapshot.CPUPeriodsTotal)
	require.Equal(t, uint64(2), latest.Snapshot.CPUThrottledPeriodsTotal)
	require.Equal(t, uint64(2), latest.Snapshot.MemoryFailuresTotal)
	require.Equal(t, uint64(6144), latest.Snapshot.MemoryCurrentBytes)
	require.Equal(t, 1, store.syncs)
}

func TestHostSandboxSamplerUsesPersistedAssignmentBaselineWithoutResamplingIt(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	cb.CGroupPath = "/cube_sandbox/sandbox/13"
	cb.RestoreHostMetricsBaseline(&cubeboxstore.HostMetricsBaseline{
		CGroupPath:          cb.CGroupPath,
		CPUUsageTotalNS:     100,
		CPUUserTotalNS:      60,
		CPUSystemTotalNS:    40,
		MemoryFailuresTotal: 3,
	})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeHostSandboxReader{usage: handle.UsageSnapshot{
		CPUUsageTotalNS:     175,
		CPUUserTotalNS:      100,
		CPUSystemTotalNS:    75,
		MemoryCurrentBytes:  4096,
		MemoryFailuresTotal: 5,
	}}
	sampler := testHostSandboxSampler(t, store, reader)

	sampleHostDirect(sampler, cb)

	latest, ok := sampler.Latest(cb.ID, time.Now())
	require.True(t, ok)
	require.Equal(t, HostSandboxAvailable, latest.Availability)
	require.Equal(t, uint64(75), latest.Snapshot.CPUUsageTotalNS)
	require.Equal(t, uint64(40), latest.Snapshot.CPUUserTotalNS)
	require.Equal(t, uint64(35), latest.Snapshot.CPUSystemTotalNS)
	require.Equal(t, uint64(2), latest.Snapshot.MemoryFailuresTotal)
	require.Equal(t, uint64(4096), latest.Snapshot.MemoryCurrentBytes)
	require.Zero(t, store.syncs)
}

func TestHostSandboxSamplerFailsClosedWhenAssignmentBaselineWasNotCaptured(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	cb.CGroupPath = "/cube_sandbox/sandbox/14"
	cb.HostMetricsBaselineMissingAtAssignment = true
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeHostSandboxReader{usage: handle.UsageSnapshot{CPUUsageTotalNS: 175}}
	sampler := testHostSandboxSampler(t, store, reader)

	sampleHostDirect(sampler, cb)

	latest, ok := sampler.Latest(cb.ID, time.Now())
	require.True(t, ok)
	require.Equal(t, HostSandboxUnavailable, latest.Availability)
	require.Nil(t, latest.Snapshot)
	require.Contains(t, latest.LastError, "assignment baseline was not captured")
	require.Empty(t, reader.requestedGroups())
	require.Zero(t, store.syncs)
}

func TestHostSandboxSamplerRetainsHistoryUntilItBecomesStaleAfterReadFailure(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	cb.CGroupPath = "/cube_sandbox/sandbox/8"
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeHostSandboxReader{usage: handle.UsageSnapshot{CPUUsageTotalNS: 100}}
	sampler := testHostSandboxSampler(t, store, reader)
	sampler.now = func() time.Time { return time.Unix(10, 0) }
	sampleHostDirect(sampler, cb)

	reader.mu.Lock()
	reader.err = errors.New("host cgroup missing")
	reader.mu.Unlock()
	sampleHostDirect(sampler, cb)
	latest, ok := sampler.Latest(cb.ID, time.Unix(11, 0))
	require.True(t, ok)
	require.Equal(t, HostSandboxAvailable, latest.Availability)
	require.NotNil(t, latest.Snapshot)
	require.Zero(t, latest.Snapshot.CPUUsageTotalNS)
	require.Contains(t, latest.LastError, "host cgroup missing")
	stale, ok := sampler.Latest(cb.ID, time.Unix(20, 0))
	require.True(t, ok)
	require.Equal(t, HostSandboxStale, stale.Availability)
	require.Contains(t, stale.LastError, "host cgroup missing")
}

func TestHostSandboxSamplerSkipsPausedSandboxAndResumesSameCounters(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{PausedAt: 1})
	cb.CGroupPath = "/cube_sandbox/sandbox/9"
	cb.RestoreHostMetricsBaseline(&cubeboxstore.HostMetricsBaseline{
		CGroupPath:      cb.CGroupPath,
		CPUUsageTotalNS: 100,
	})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeHostSandboxReader{usage: handle.UsageSnapshot{CPUUsageTotalNS: 150}}
	sampler := testHostSandboxSampler(t, store, reader)

	sampler.CollectOnce(context.Background())
	latest, ok := sampler.Latest(cb.ID, time.Now())
	require.True(t, ok)
	require.Equal(t, HostSandboxUnavailable, latest.Availability)
	require.Empty(t, reader.requestedGroups())

	require.NoError(t, cb.FirstContainer().Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
		status.PausedAt = 0
		status.StartedAt = 2
		return status, nil
	}))
	sampler.SetLifecycleUnavailable(cb.ID, false)
	sampleHostDirect(sampler, cb)
	latest, ok = sampler.Latest(cb.ID, time.Now())
	require.True(t, ok)
	require.Equal(t, HostSandboxAvailable, latest.Availability)
	require.Equal(t, uint64(50), latest.Snapshot.CPUUsageTotalNS)
	require.Equal(t, uint64(1), cb.GuestMetricsEpochCopy().Generation)
}

func TestHostSandboxSamplerDoesNotExposeReusedCountersWhenBaselinePersistenceFails(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	cb.CGroupPath = "/cube_sandbox/sandbox/11"
	store := &fakeGuestWorkloadStore{
		boxes:   []*cubeboxstore.CubeBox{cb},
		syncErr: errors.New("metadata unavailable"),
	}
	reader := &fakeHostSandboxReader{usage: handle.UsageSnapshot{
		CPUUsageTotalNS:     4_800_000_000_000,
		MemoryCurrentBytes:  4096,
		MemoryFailuresTotal: 7,
	}}
	sampler := testHostSandboxSampler(t, store, reader)

	sampleHostDirect(sampler, cb)
	require.Nil(t, cb.HostMetricsBaselineCopy())
	latest, ok := sampler.Latest(cb.ID, time.Now())
	require.True(t, ok)
	require.Equal(t, HostSandboxUnavailable, latest.Availability)
	require.Nil(t, latest.Snapshot)
	require.Contains(t, latest.LastError, "persist host sandbox counter baseline")
}

func TestNormalizeHostSandboxUsageRejectsCounterRegression(t *testing.T) {
	_, err := normalizeHostSandboxUsage(
		handle.UsageSnapshot{CPUUsageTotalNS: 99},
		cubeboxstore.HostMetricsBaseline{
			CGroupPath:      "/cube_sandbox/sandbox/12",
			CPUUsageTotalNS: 100,
		},
	)
	require.ErrorContains(t, err, "CPU usage counter regressed")
}

func TestHostSandboxSamplerDiscardsInFlightResultAfterDeletion(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	cb.CGroupPath = "/cube_sandbox/sandbox/10"
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	release := make(chan struct{})
	reader := &fakeHostSandboxReader{
		usage:   handle.UsageSnapshot{CPUUsageTotalNS: 100},
		started: make(chan struct{}, 1),
		release: release,
	}
	sampler := testHostSandboxSampler(t, store, reader)

	sampler.CollectOnce(context.Background())
	<-reader.started
	store.boxes = nil
	sampler.CollectOnce(context.Background())
	close(release)
	require.Eventually(t, func() bool {
		sampler.mu.RLock()
		defer sampler.mu.RUnlock()
		return len(sampler.inFlight) == 0
	}, time.Second, time.Millisecond)
	_, ok := sampler.Latest(cb.ID, time.Now())
	require.False(t, ok)
}

func TestHostSandboxSamplerDispatchIntervalIgnoresPausedSandboxes(t *testing.T) {
	boxes := make([]*cubeboxstore.CubeBox, 16)
	for i := range boxes {
		status := cubeboxstore.Status{PausedAt: 1}
		if i == 0 {
			status = cubeboxstore.Status{StartedAt: 1}
		}
		boxes[i] = testSamplerCubeBoxWithID(t, fmt.Sprintf("sandbox-%02d", i), status)
		boxes[i].CGroupPath = fmt.Sprintf("/cube_sandbox/sandbox/%d", i)
	}
	sampler := testHostSandboxSampler(t, &fakeGuestWorkloadStore{boxes: boxes}, &fakeHostSandboxReader{})

	require.Equal(t, time.Second, sampler.nextDispatchInterval())
}

func testHostSandboxSampler(t *testing.T, store guestWorkloadCubeboxStore, reader hostSandboxUsageReader) *HostSandboxSampler {
	t.Helper()
	sampler, err := NewHostSandboxSampler(HostSandboxSamplerConfig{
		CollectionInterval:    time.Second,
		RequestTimeout:        100 * time.Millisecond,
		MaxConcurrentRequests: 1,
		StaleAfter:            5 * time.Second,
	}, store, reader)
	require.NoError(t, err)
	return sampler
}

func sampleHostDirect(sampler *HostSandboxSampler, cb *cubeboxstore.CubeBox) {
	sampler.requests <- struct{}{}
	token, _ := sampler.start(cb.ID)
	sampler.collect(context.Background(), cb, token)
}

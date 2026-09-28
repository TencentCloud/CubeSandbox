package resourcemetrics

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/stretchr/testify/require"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	cubeboxapi "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

type fakeGuestWorkloadStore struct {
	boxes       []*cubeboxstore.CubeBox
	syncErr     error
	syncs       int
	syncStarted chan struct{}
	syncRelease <-chan struct{}
	listCalls   atomic.Int64
}

func (s *fakeGuestWorkloadStore) List() []*cubeboxstore.CubeBox {
	s.listCalls.Add(1)
	return s.boxes
}

func (s *fakeGuestWorkloadStore) listCount() int64 { return s.listCalls.Load() }

func (s *fakeGuestWorkloadStore) SyncByID(_ context.Context, _ string) error {
	s.syncs++
	if s.syncStarted != nil {
		s.syncStarted <- struct{}{}
	}
	if s.syncRelease != nil {
		<-s.syncRelease
	}
	return s.syncErr
}

type fakeGuestWorkloadReader struct {
	mu      sync.Mutex
	metrics []*types.Metric
	err     error
	calls   int
	ids     []string
	started chan struct{}
	release <-chan struct{}
}

type guestWorkloadReaderFunc func(context.Context, string) (*types.Metric, error)

func (f guestWorkloadReaderFunc) Metrics(ctx context.Context, sandboxID string) (*types.Metric, error) {
	return f(ctx, sandboxID)
}

func (r *fakeGuestWorkloadReader) Metrics(_ context.Context, sandboxID string) (*types.Metric, error) {
	r.mu.Lock()
	r.calls++
	r.ids = append(r.ids, sandboxID)
	index := r.calls - 1
	r.mu.Unlock()
	if r.started != nil {
		r.started <- struct{}{}
	}
	if r.release != nil {
		<-r.release
	}
	if r.err != nil {
		return nil, r.err
	}
	return r.metrics[index], nil
}

func (r *fakeGuestWorkloadReader) requestedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

func TestGuestWorkloadSamplerPersistsBaselineAndNormalizesLaterSamples(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeGuestWorkloadReader{metrics: []*types.Metric{
		testContainerdMetric(t, time.Unix(10, 0), testGuestValues{cpuTotal: 100, cpuUser: 60, cpuSystem: 40, throttledTime: 10, periods: 8, throttledPeriods: 2, memoryCurrent: 4096, memoryLimit: 16384, memoryFailures: 3}),
		testContainerdMetric(t, time.Unix(15, 0), testGuestValues{cpuTotal: 175, cpuUser: 100, cpuSystem: 75, throttledTime: 19, periods: 11, throttledPeriods: 4, memoryCurrent: 6144, memoryLimit: 16384, memoryFailures: 5}),
	}}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

	sampleDirect(sampler, cb, workload)
	epoch := cb.GuestMetricsEpochCopy()
	require.Equal(t, cubeboxstore.GuestMetricsEpochReady, epoch.State)
	require.Equal(t, uint64(100), epoch.Baseline.CPUUsageTotalNS)
	require.Equal(t, 1, store.syncs)
	first, ok := sampler.Latest(workload, time.Unix(11, 0))
	require.True(t, ok)
	require.True(t, first.CumulativeAvailable)
	require.Equal(t, uint64(0), first.Snapshot.Counters.CPUUsageTotalNS)
	require.Equal(t, uint64(4096), first.MemoryCurrentBytes)

	sampleDirect(sampler, cb, workload)
	second, ok := sampler.Latest(workload, time.Unix(16, 0))
	require.True(t, ok)
	require.Equal(t, uint64(75), second.Snapshot.Counters.CPUUsageTotalNS)
	require.Equal(t, uint64(40), second.Snapshot.Counters.CPUUserTotalNS)
	require.Equal(t, uint64(35), second.Snapshot.Counters.CPUSystemTotalNS)
	require.Equal(t, uint64(2), second.Snapshot.Counters.MemoryFailuresTotal)
	require.Equal(t, uint64(6144), second.MemoryCurrentBytes)
}

func TestGuestWorkloadSamplerRecoversMissingFreshEpoch(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	cb.RestoreGuestMetricsEpoch(nil)
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeGuestWorkloadReader{metrics: []*types.Metric{
		testContainerdMetric(t, time.Unix(10, 0), testGuestValues{cpuTotal: 100, memoryCurrent: 4096, memoryLimit: 16384}),
	}}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

	sampler.CollectOnce(context.Background())
	require.Eventually(t, func() bool {
		latest, ok := sampler.Latest(workload, time.Unix(11, 0))
		return ok && latest.Availability == GuestWorkloadAvailable
	}, time.Second, 10*time.Millisecond)

	epoch := cb.GuestMetricsEpochCopy()
	require.NotNil(t, epoch)
	require.Equal(t, cubeboxstore.GuestMetricsEpochFreshCreate, epoch.Reason)
	require.Equal(t, cubeboxstore.GuestMetricsEpochReady, epoch.State)
	require.Equal(t, 2, store.syncs)
	require.Equal(t, []string{cb.ID}, reader.requestedIDs())
}

func TestGuestWorkloadSamplerRetriesMissingFreshEpochPersistence(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	cb.RestoreGuestMetricsEpoch(nil)
	store := &fakeGuestWorkloadStore{
		boxes:   []*cubeboxstore.CubeBox{cb},
		syncErr: errors.New("metadata temporarily unavailable"),
	}
	reader := &fakeGuestWorkloadReader{metrics: []*types.Metric{
		testContainerdMetric(t, time.Unix(10, 0), testGuestValues{cpuTotal: 100, memoryLimit: 16384}),
	}}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

	sampler.CollectOnce(context.Background())
	require.Nil(t, cb.GuestMetricsEpochCopy())
	latest, ok := sampler.Latest(workload, time.Unix(11, 0))
	require.True(t, ok)
	require.Contains(t, latest.LastError, "persist missing fresh guest workload metrics epoch")
	require.Empty(t, reader.requestedIDs())

	store.syncErr = nil
	sampler.CollectOnce(context.Background())
	require.Eventually(t, func() bool {
		latest, ok := sampler.Latest(workload, time.Unix(11, 0))
		return ok && latest.Availability == GuestWorkloadAvailable
	}, time.Second, 10*time.Millisecond)

	epoch := cb.GuestMetricsEpochCopy()
	require.NotNil(t, epoch)
	require.Equal(t, cubeboxstore.GuestMetricsEpochReady, epoch.State)
	require.Equal(t, 3, store.syncs)
	require.Equal(t, []string{cb.ID}, reader.requestedIDs())
}

func TestGuestWorkloadSamplerUsesCubeboxNamespaceForMetricsRequest(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	cb.Namespace = "tenant-a"
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	requestedNamespace := make(chan string, 1)
	reader := guestWorkloadReaderFunc(func(ctx context.Context, _ string) (*types.Metric, error) {
		namespace, err := namespaces.NamespaceRequired(ctx)
		if err != nil {
			return nil, err
		}
		requestedNamespace <- namespace
		return testContainerdMetric(t, time.Unix(10, 0), testGuestValues{memoryLimit: 16384}), nil
	})
	sampler := testGuestWorkloadSampler(t, store, reader)

	sampler.CollectOnce(context.Background())

	select {
	case namespace := <-requestedNamespace:
		require.Equal(t, "tenant-a", namespace)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for guest metrics request")
	}
}

func TestGuestWorkloadSamplerPublishesConfiguredCPULimit(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	cb.FirstContainer().Config = &cubeboxapi.ContainerConfig{Resources: &cubeboxapi.Resource{CpuLimit: "750m"}}
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeGuestWorkloadReader{metrics: []*types.Metric{
		testContainerdMetric(t, time.Unix(10, 0), testGuestValues{memoryLimit: 16384}),
	}}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

	sampleDirect(sampler, cb, workload)
	latest, ok := sampler.Latest(workload, time.Unix(11, 0))
	require.True(t, ok)
	require.NotNil(t, latest.CPULimitMillicores)
	require.Equal(t, int64(750), *latest.CPULimitMillicores)
	require.NotNil(t, latest.Snapshot.CPULimitMillicores)
	require.Equal(t, int64(750), *latest.Snapshot.CPULimitMillicores)
}

func TestGuestWorkloadSamplerSnapshotsCPULimitBeforeAsyncCollection(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	cb.FirstContainer().Config = &cubeboxapi.ContainerConfig{Resources: &cubeboxapi.Resource{CpuLimit: "750m"}}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeGuestWorkloadReader{
		metrics: []*types.Metric{
			testContainerdMetric(t, time.Unix(10, 0), testGuestValues{memoryLimit: 16384}),
		},
		started: started,
		release: release,
	}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

	sampler.CollectOnce(context.Background())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for guest metrics request")
	}
	cb.FirstContainer().Config = &cubeboxapi.ContainerConfig{Resources: &cubeboxapi.Resource{CpuLimit: "1500m"}}
	close(release)

	require.Eventually(t, func() bool {
		latest, ok := sampler.Latest(workload, time.Unix(11, 0))
		return ok && latest.CPULimitMillicores != nil
	}, time.Second, 10*time.Millisecond)
	latest, ok := sampler.Latest(workload, time.Unix(11, 0))
	require.True(t, ok)
	require.Equal(t, int64(750), *latest.CPULimitMillicores)
}

func TestGuestWorkloadSamplerKeepsPendingMemorySnapshotWhenBaselinePersistenceFails(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}, syncErr: errors.New("metadata unavailable")}
	reader := &fakeGuestWorkloadReader{metrics: []*types.Metric{
		testContainerdMetric(t, time.Unix(10, 0), testGuestValues{cpuTotal: 100, memoryCurrent: 4096, memoryLimit: 16384}),
	}}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

	sampleDirect(sampler, cb, workload)
	require.Equal(t, cubeboxstore.GuestMetricsEpochPending, cb.GuestMetricsEpochCopy().State)
	latest, ok := sampler.Latest(workload, time.Unix(11, 0))
	require.True(t, ok)
	require.False(t, latest.CumulativeAvailable)
	require.Nil(t, latest.Snapshot)
	require.Equal(t, uint64(4096), latest.MemoryCurrentBytes)
	require.Contains(t, latest.LastError, "persist guest workload epoch baseline")
}

func TestGuestWorkloadSamplerUsesPersistedReadyBaselineWithoutResync(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	require.NoError(t, cb.ReadyGuestMetricsEpoch(1, cubeboxstore.GuestMetricsEpochBaseline{
		ContainerID:     cb.ID,
		CPUUsageTotalNS: 100,
	}, time.Unix(2, 0)))
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeGuestWorkloadReader{metrics: []*types.Metric{
		testContainerdMetric(t, time.Unix(10, 0), testGuestValues{cpuTotal: 175, memoryLimit: 16384}),
	}}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

	sampleDirect(sampler, cb, workload)
	latest, ok := sampler.Latest(workload, time.Unix(11, 0))
	require.True(t, ok)
	require.Equal(t, uint64(75), latest.Snapshot.Counters.CPUUsageTotalNS)
	require.Equal(t, 0, store.syncs)
}

func TestGuestWorkloadSamplerDegradesCounterRegression(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeGuestWorkloadReader{metrics: []*types.Metric{
		testContainerdMetric(t, time.Unix(10, 0), testGuestValues{cpuTotal: 100, memoryCurrent: 4096, memoryLimit: 16384}),
		testContainerdMetric(t, time.Unix(11, 0), testGuestValues{cpuTotal: 99, memoryCurrent: 6144, memoryLimit: 16384}),
	}}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

	sampleDirect(sampler, cb, workload)
	sampleDirect(sampler, cb, workload)
	require.Equal(t, cubeboxstore.GuestMetricsEpochDegraded, cb.GuestMetricsEpochCopy().State)
	latest, ok := sampler.Latest(workload, time.Unix(12, 0))
	require.True(t, ok)
	require.Equal(t, GuestWorkloadUnavailable, latest.Availability)
	require.False(t, latest.CumulativeAvailable)
	require.Nil(t, latest.Snapshot)
	require.Contains(t, latest.LastError, "counter regressed")
}

func TestGuestWorkloadSamplerSkipsPausedWorkloads(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{PausedAt: 1})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeGuestWorkloadReader{}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

	sampler.CollectOnce(context.Background())
	latest, ok := sampler.Latest(workload, time.Now())
	require.True(t, ok)
	require.Equal(t, GuestWorkloadUnavailable, latest.Availability)
	require.Equal(t, 0, reader.calls)
}

func TestGuestWorkloadSamplerBoundsRequestTimeout(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := guestWorkloadReaderFunc(func(ctx context.Context, _ string) (*types.Metric, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	sampler, err := NewGuestWorkloadSampler(GuestWorkloadSamplerConfig{
		CollectionInterval:    time.Second,
		RequestTimeout:        10 * time.Millisecond,
		MaxConcurrentRequests: 1,
		StaleAfter:            5 * time.Second,
	}, store, reader)
	require.NoError(t, err)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

	sampler.CollectOnce(context.Background())
	require.Eventually(t, func() bool {
		latest, ok := sampler.Latest(workload, time.Now())
		return ok && latest.Availability == GuestWorkloadUnavailable && latest.LastError == context.DeadlineExceeded.Error()
	}, time.Second, time.Millisecond)
}

func TestGuestWorkloadSamplerRetainsLastSuccessUntilTransportFailureBecomesStale(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeGuestWorkloadReader{metrics: []*types.Metric{
		testContainerdMetric(t, time.Unix(10, 0), testGuestValues{cpuTotal: 100, memoryLimit: 16384}),
	}, err: nil}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}
	sampleDirect(sampler, cb, workload)

	reader.err = errors.New("guest task unavailable")
	sampler.CollectOnce(context.Background())
	require.Eventually(t, func() bool {
		latest, ok := sampler.Latest(workload, time.Unix(11, 0))
		return ok && latest.Availability == GuestWorkloadAvailable && latest.LastError == "guest task unavailable"
	}, time.Second, time.Millisecond)
	latest, ok := sampler.Latest(workload, time.Unix(11, 0))
	require.True(t, ok)
	require.NotNil(t, latest.Snapshot)
	require.True(t, latest.CumulativeAvailable)
	require.Contains(t, latest.LastError, "guest task unavailable")

	stale, ok := sampler.Latest(workload, time.Unix(16, 0))
	require.True(t, ok)
	require.Equal(t, GuestWorkloadStale, stale.Availability)
	require.NotNil(t, stale.Snapshot)
	require.True(t, stale.CumulativeAvailable)
}

func TestGuestWorkloadSamplerFailsClosedImmediatelyOnCapabilityError(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeGuestWorkloadReader{metrics: []*types.Metric{
		testContainerdMetric(t, time.Unix(10, 0), testGuestValues{cpuTotal: 100, memoryLimit: 16384}),
	}}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}
	sampleDirect(sampler, cb, workload)

	reader.err = fmt.Errorf("%w: guest resource metrics capability is unavailable", errdefs.ErrFailedPrecondition)
	sampler.CollectOnce(context.Background())
	require.Eventually(t, func() bool {
		latest, ok := sampler.Latest(workload, time.Unix(11, 0))
		return ok && latest.Availability == GuestWorkloadUnavailable
	}, time.Second, time.Millisecond)
	latest, ok := sampler.Latest(workload, time.Unix(11, 0))
	require.True(t, ok)
	require.Contains(t, latest.LastError, "guest resource metrics capability is unavailable")
}

func TestGuestWorkloadSamplerDoesNotReadPreparedRollbackEpoch(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	require.NoError(t, cb.PrepareRollbackGuestMetricsEpoch(time.Unix(9, 0)))
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeGuestWorkloadReader{metrics: []*types.Metric{
		testContainerdMetric(t, time.Unix(10, 0), testGuestValues{cpuTotal: 100}),
	}}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

	sampler.CollectOnce(context.Background())
	latest, ok := sampler.Latest(workload, time.Unix(11, 0))
	require.True(t, ok)
	require.Equal(t, uint64(2), latest.EpochGeneration)
	require.Equal(t, GuestWorkloadUnavailable, latest.Availability)
	require.Zero(t, reader.calls)
}

func TestGuestWorkloadSamplerLimitsOneInFlightRequestPerWorkload(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	release := make(chan struct{})
	reader := &fakeGuestWorkloadReader{
		metrics: []*types.Metric{testContainerdMetric(t, time.Unix(10, 0), testGuestValues{memoryLimit: 16384})},
		started: make(chan struct{}, 1),
		release: release,
	}
	sampler := testGuestWorkloadSampler(t, store, reader)

	sampler.CollectOnce(context.Background())
	<-reader.started
	sampler.CollectOnce(context.Background())
	require.Equal(t, 1, reader.calls)
	close(release)
	require.Eventually(t, func() bool { return reader.calls == 1 }, time.Second, time.Millisecond)
}

func TestGuestWorkloadSamplerContinuesAfterConcurrencySaturation(t *testing.T) {
	first := testSamplerCubeBoxWithID(t, "a", cubeboxstore.Status{StartedAt: 1})
	second := testSamplerCubeBoxWithID(t, "b", cubeboxstore.Status{StartedAt: 1})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{second, first}}
	release := make(chan struct{}, 2)
	firstMetric := testContainerdMetric(t, time.Unix(10, 0), testGuestValues{memoryLimit: 16384})
	firstMetric.ID = "a"
	secondMetric := testContainerdMetric(t, time.Unix(11, 0), testGuestValues{memoryLimit: 16384})
	secondMetric.ID = "b"
	reader := &fakeGuestWorkloadReader{
		metrics: []*types.Metric{firstMetric, secondMetric},
		started: make(chan struct{}, 2),
		release: release,
	}
	sampler := testGuestWorkloadSampler(t, store, reader)

	sampler.CollectOnce(context.Background())
	<-reader.started
	release <- struct{}{}
	require.Eventually(t, func() bool {
		sampler.mu.RLock()
		defer sampler.mu.RUnlock()
		return len(sampler.inFlight) == 0
	}, time.Second, time.Millisecond)
	sampler.CollectOnce(context.Background())
	<-reader.started
	release <- struct{}{}
	require.Eventually(t, func() bool {
		return len(reader.requestedIDs()) == 2
	}, time.Second, time.Millisecond)
	require.Equal(t, []string{"a", "b"}, reader.requestedIDs())
}

func TestGuestWorkloadSamplerSignalsCompletionAfterSaturation(t *testing.T) {
	first := testSamplerCubeBoxWithID(t, "a", cubeboxstore.Status{StartedAt: 1})
	second := testSamplerCubeBoxWithID(t, "b", cubeboxstore.Status{StartedAt: 1})
	release := make(chan struct{})
	metric := testContainerdMetric(t, time.Unix(10, 0), testGuestValues{memoryLimit: 16384})
	metric.ID = "a"
	reader := &fakeGuestWorkloadReader{
		metrics: []*types.Metric{metric},
		started: make(chan struct{}, 1),
		release: release,
	}
	sampler := testGuestWorkloadSampler(t, &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{first, second}}, reader)

	require.True(t, sampler.CollectOnce(context.Background()))
	<-reader.started
	select {
	case <-sampler.completed:
		t.Fatal("completion must not be signaled while the request is still in flight")
	default:
	}
	close(release)
	select {
	case <-sampler.completed:
	case <-time.After(time.Second):
		t.Fatal("saturated scheduler was not notified when request capacity became available")
	}
}

func TestGuestWorkloadSamplerSpreadsBatchesAcrossCollectionInterval(t *testing.T) {
	boxes := make([]*cubeboxstore.CubeBox, 16)
	for i := range boxes {
		boxes[i] = testSamplerCubeBoxWithID(t, fmt.Sprintf("sandbox-%02d", i), cubeboxstore.Status{StartedAt: 1})
	}
	store := &fakeGuestWorkloadStore{boxes: boxes}
	reader := &fakeGuestWorkloadReader{}
	sampler := testGuestWorkloadSampler(t, store, reader)

	require.Equal(t, 62*time.Millisecond+500*time.Microsecond, sampler.nextDispatchInterval())
	for i := 0; i < 100; i++ {
		dispatch := sampler.jitter(sampler.nextDispatchInterval())
		require.GreaterOrEqual(t, dispatch, 56*time.Millisecond)
		require.LessOrEqual(t, dispatch, 69*time.Millisecond)
	}
}

func TestGuestWorkloadSamplerDispatchIntervalIgnoresPausedSandboxes(t *testing.T) {
	boxes := make([]*cubeboxstore.CubeBox, 16)
	for i := range boxes {
		status := cubeboxstore.Status{PausedAt: 1}
		if i == 0 {
			status = cubeboxstore.Status{StartedAt: 1}
		}
		boxes[i] = testSamplerCubeBoxWithID(t, fmt.Sprintf("sandbox-%02d", i), status)
	}
	sampler := testGuestWorkloadSampler(t, &fakeGuestWorkloadStore{boxes: boxes}, &fakeGuestWorkloadReader{})

	require.Equal(t, time.Second, sampler.nextDispatchInterval())
}

func TestGuestWorkloadSamplerMarksOldSampleStale(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeGuestWorkloadReader{metrics: []*types.Metric{
		testContainerdMetric(t, time.Unix(10, 0), testGuestValues{memoryLimit: 16384}),
	}}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

	sampleDirect(sampler, cb, workload)
	latest, ok := sampler.Latest(workload, time.Unix(30, 0))
	require.True(t, ok)
	require.Equal(t, GuestWorkloadStale, latest.Availability)
}

func TestGuestWorkloadSamplerInvalidatesCacheWhenEpochChanges(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeGuestWorkloadReader{metrics: []*types.Metric{
		testContainerdMetric(t, time.Unix(10, 0), testGuestValues{cpuTotal: 100, memoryLimit: 16384}),
		testContainerdMetric(t, time.Unix(20, 0), testGuestValues{cpuTotal: 200, memoryLimit: 16384}),
	}}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

	sampleDirect(sampler, cb, workload)
	require.Equal(t, uint64(1), cb.GuestMetricsEpochCopy().Generation)
	require.NoError(t, cb.BeginGuestMetricsEpoch(cubeboxstore.GuestMetricsEpochRollback, time.Unix(15, 0)))
	sampleDirect(sampler, cb, workload)
	latest, ok := sampler.Latest(workload, time.Unix(21, 0))
	require.True(t, ok)
	require.Equal(t, uint64(2), latest.EpochGeneration)
	require.True(t, latest.CumulativeAvailable)
	require.Equal(t, uint64(0), latest.Snapshot.Counters.CPUUsageTotalNS)
}

func TestGuestWorkloadSamplerDiscardsSampleFromPreviousEpoch(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	release := make(chan struct{})
	reader := &fakeGuestWorkloadReader{
		metrics: []*types.Metric{testContainerdMetric(t, time.Unix(10, 0), testGuestValues{cpuTotal: 100, memoryLimit: 16384})},
		started: make(chan struct{}, 1),
		release: release,
	}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

	sampler.CollectOnce(context.Background())
	<-reader.started
	require.NoError(t, cb.BeginGuestMetricsEpoch(cubeboxstore.GuestMetricsEpochRollback, time.Unix(9, 0)))
	sampler.InvalidateEpoch(workload, cb.GuestMetricsEpochCopy())
	close(release)
	require.Eventually(t, func() bool {
		sampler.mu.RLock()
		defer sampler.mu.RUnlock()
		return len(sampler.inFlight) == 0
	}, time.Second, time.Millisecond)

	epoch := cb.GuestMetricsEpochCopy()
	require.Equal(t, uint64(2), epoch.Generation)
	require.Equal(t, cubeboxstore.GuestMetricsEpochPending, epoch.State)
	require.Nil(t, epoch.Baseline)
	latest, ok := sampler.Latest(workload, time.Unix(11, 0))
	require.True(t, ok)
	require.Equal(t, uint64(2), latest.EpochGeneration)
	require.False(t, latest.CumulativeAvailable)
	require.Nil(t, latest.Snapshot)
}

func TestGuestWorkloadSamplerDiscardsErrorFromPreviousEpoch(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	release := make(chan struct{})
	reader := &fakeGuestWorkloadReader{
		err:     errors.New("old epoch transport failure"),
		started: make(chan struct{}, 1),
		release: release,
	}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

	sampler.CollectOnce(context.Background())
	<-reader.started
	require.NoError(t, cb.BeginGuestMetricsEpoch(cubeboxstore.GuestMetricsEpochRollback, time.Unix(9, 0)))
	sampler.InvalidateEpoch(workload, cb.GuestMetricsEpochCopy())
	close(release)
	require.Eventually(t, func() bool {
		sampler.mu.RLock()
		defer sampler.mu.RUnlock()
		return len(sampler.inFlight) == 0
	}, time.Second, time.Millisecond)

	latest, ok := sampler.Latest(workload, time.Unix(11, 0))
	require.True(t, ok)
	require.Equal(t, uint64(2), latest.EpochGeneration)
	require.NotContains(t, latest.LastError, "old epoch transport failure")
}

func TestGuestWorkloadSamplerPauseWinsOverInFlightSample(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	release := make(chan struct{})
	reader := &fakeGuestWorkloadReader{
		metrics: []*types.Metric{testContainerdMetric(t, time.Unix(10, 0), testGuestValues{cpuTotal: 100, memoryLimit: 16384})},
		started: make(chan struct{}, 1),
		release: release,
	}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

	sampler.CollectOnce(context.Background())
	<-reader.started
	require.NoError(t, cb.FirstContainer().Status.Update(func(status cubeboxstore.Status) (cubeboxstore.Status, error) {
		status.PausedAt = 1
		return status, nil
	}))
	sampler.SetLifecycleUnavailable(workload, true)
	close(release)
	require.Eventually(t, func() bool {
		latest, ok := sampler.Latest(workload, time.Unix(11, 0))
		return ok && latest.Snapshot != nil
	}, time.Second, time.Millisecond)
	latest, ok := sampler.Latest(workload, time.Unix(11, 0))
	require.True(t, ok)
	require.Equal(t, GuestWorkloadUnavailable, latest.Availability)
	require.False(t, latest.CumulativeAvailable)
}

func TestGuestWorkloadSamplerPersistenceFailureDoesNotRestoreOlderEpoch(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	releaseSync := make(chan struct{})
	store := &fakeGuestWorkloadStore{
		boxes:       []*cubeboxstore.CubeBox{cb},
		syncErr:     errors.New("metadata unavailable"),
		syncStarted: make(chan struct{}, 1),
		syncRelease: releaseSync,
	}
	reader := &fakeGuestWorkloadReader{metrics: []*types.Metric{
		testContainerdMetric(t, time.Unix(10, 0), testGuestValues{cpuTotal: 100, memoryLimit: 16384}),
	}}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

	sampler.CollectOnce(context.Background())
	<-store.syncStarted
	require.NoError(t, cb.BeginGuestMetricsEpoch(cubeboxstore.GuestMetricsEpochRollback, time.Unix(11, 0)))
	sampler.InvalidateEpoch(workload, cb.GuestMetricsEpochCopy())
	close(releaseSync)
	require.Eventually(t, func() bool {
		sampler.mu.RLock()
		defer sampler.mu.RUnlock()
		return len(sampler.inFlight) == 0
	}, time.Second, time.Millisecond)

	epoch := cb.GuestMetricsEpochCopy()
	require.Equal(t, uint64(2), epoch.Generation)
	require.Equal(t, cubeboxstore.GuestMetricsEpochPending, epoch.State)
	require.Nil(t, epoch.Baseline)
	latest, ok := sampler.Latest(workload, time.Unix(12, 0))
	require.True(t, ok)
	require.Equal(t, uint64(2), latest.EpochGeneration)
	require.Nil(t, latest.Snapshot)
}

func TestGuestWorkloadSamplerRemovesDeletedWorkloadCache(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	reader := &fakeGuestWorkloadReader{metrics: []*types.Metric{
		testContainerdMetric(t, time.Unix(10, 0), testGuestValues{memoryLimit: 16384}),
	}}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}
	sampleDirect(sampler, cb, workload)

	store.boxes = nil
	sampler.CollectOnce(context.Background())
	_, ok := sampler.Latest(workload, time.Unix(11, 0))
	require.False(t, ok)
}

func TestGuestWorkloadSamplerDiscardsInFlightResultAfterDeletion(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	release := make(chan struct{})
	reader := &fakeGuestWorkloadReader{
		metrics: []*types.Metric{testContainerdMetric(t, time.Unix(10, 0), testGuestValues{memoryLimit: 16384})},
		started: make(chan struct{}, 1),
		release: release,
	}
	sampler := testGuestWorkloadSampler(t, store, reader)
	workload := WorkloadRef{SandboxID: cb.ID, ContainerID: cb.ID}

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
	_, ok := sampler.Latest(workload, time.Unix(11, 0))
	require.False(t, ok)
}

func TestGuestWorkloadSamplerListsCopiedCacheInStableOrder(t *testing.T) {
	store := &fakeGuestWorkloadStore{}
	reader := &fakeGuestWorkloadReader{}
	sampler := testGuestWorkloadSampler(t, store, reader)
	sampler.record(0, GuestWorkloadLatest{
		Workload:     WorkloadRef{SandboxID: "b", ContainerID: "b"},
		CollectedAt:  time.Unix(10, 0),
		Availability: GuestWorkloadAvailable,
		Snapshot:     &GuestWorkloadSnapshot{BaselineID: "b"},
	})
	sampler.record(0, GuestWorkloadLatest{
		Workload:     WorkloadRef{SandboxID: "a", ContainerID: "a"},
		CollectedAt:  time.Unix(10, 0),
		Availability: GuestWorkloadAvailable,
		Snapshot:     &GuestWorkloadSnapshot{BaselineID: "a"},
	})

	got := sampler.ListLatest(time.Unix(20, 0))
	require.Equal(t, []string{"a", "b"}, []string{got[0].Workload.SandboxID, got[1].Workload.SandboxID})
	require.Equal(t, GuestWorkloadStale, got[0].Availability)
	got[0].Snapshot.BaselineID = "mutated"
	require.Equal(t, "a", sampler.ListLatest(time.Unix(11, 0))[0].Snapshot.BaselineID)
}

func testSamplerCubeBox(t *testing.T, status cubeboxstore.Status) *cubeboxstore.CubeBox {
	return testSamplerCubeBoxWithID(t, "container", status)
}

func testSamplerCubeBoxWithID(t *testing.T, id string, status cubeboxstore.Status) *cubeboxstore.CubeBox {
	t.Helper()
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: id}}
	cb.AddContainer(&cubeboxstore.Container{
		Metadata: cb.Metadata,
		Status:   cubeboxstore.StoreStatus(status),
		IsPod:    true,
	})
	require.NoError(t, cb.BeginGuestMetricsEpoch(cubeboxstore.GuestMetricsEpochFreshCreate, time.Unix(1, 0)))
	return cb
}

func testGuestWorkloadSampler(t *testing.T, store guestWorkloadCubeboxStore, reader guestWorkloadMetricsReader) *GuestWorkloadSampler {
	t.Helper()
	sampler, err := NewGuestWorkloadSampler(GuestWorkloadSamplerConfig{
		CollectionInterval:    time.Second,
		RequestTimeout:        time.Second,
		MaxConcurrentRequests: 1,
		StaleAfter:            5 * time.Second,
	}, store, reader)
	require.NoError(t, err)
	return sampler
}

func sampleDirect(sampler *GuestWorkloadSampler, cb *cubeboxstore.CubeBox, workload WorkloadRef) {
	sampler.requests <- struct{}{}
	token, _ := sampler.start(workload)
	namespace := cb.Namespace
	if namespace == "" {
		namespace = namespaces.Default
	}
	cpuLimitMillicores, err := guestWorkloadCPULimitMillicores(cb)
	if err != nil {
		panic(err)
	}
	sampler.collect(
		context.Background(),
		cb,
		workload,
		namespace,
		cpuLimitMillicores,
		cb.GuestMetricsEpochCopy().Generation,
		token,
	)
}

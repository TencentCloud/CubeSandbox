package resourcemetrics

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/api/types"
	"github.com/stretchr/testify/require"

	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
)

func TestResourceMetricsAvailabilitySeparatesGuestRollbackFromHostLifecycle(t *testing.T) {
	require.True(t, guestWorkloadSamplingUnavailable(cubeboxstore.StoreStatus(cubeboxstore.Status{})))
	require.True(t, hostSandboxSamplingUnavailable(cubeboxstore.StoreStatus(cubeboxstore.Status{})))
	require.True(t, guestWorkloadSamplingUnavailable(cubeboxstore.StoreStatus(cubeboxstore.Status{PausedAt: 1})))
	require.True(t, hostSandboxSamplingUnavailable(cubeboxstore.StoreStatus(cubeboxstore.Status{PausedAt: 1})))
	require.True(t, guestWorkloadSamplingUnavailable(cubeboxstore.StoreStatus(cubeboxstore.Status{RollingBack: true, StartedAt: 1})))
	require.False(t, hostSandboxSamplingUnavailable(cubeboxstore.StoreStatus(cubeboxstore.Status{RollingBack: true, StartedAt: 1})))
	require.False(t, guestWorkloadSamplingUnavailable(cubeboxstore.StoreStatus(cubeboxstore.Status{StartedAt: 1})))
	require.False(t, hostSandboxSamplingUnavailable(cubeboxstore.StoreStatus(cubeboxstore.Status{StartedAt: 1})))
}

func TestWaitForSamplerDispatchKeepsMinimumIntervalAfterEarlyCompletion(t *testing.T) {
	ctxDone := make(chan struct{})
	completed := make(chan struct{}, 1)
	completed <- struct{}{}
	start := time.Now()

	require.True(t, waitForSamplerDispatch(ctxDone, 30*time.Millisecond, completed))
	require.GreaterOrEqual(t, time.Since(start), 25*time.Millisecond)
}

func TestWaitForSamplerDispatchWaitsForCompletionAfterInterval(t *testing.T) {
	ctxDone := make(chan struct{})
	completed := make(chan struct{}, 1)
	done := make(chan bool, 1)
	go func() {
		done <- waitForSamplerDispatch(ctxDone, 10*time.Millisecond, completed)
	}()

	select {
	case <-done:
		t.Fatal("dispatch returned before saturated request completed")
	case <-time.After(20 * time.Millisecond):
	}
	completed <- struct{}{}
	select {
	case ok := <-done:
		require.True(t, ok)
	case <-time.After(time.Second):
		t.Fatal("dispatch did not resume after request completion")
	}
}

func TestGuestAndHostSamplerStatusChecksDoNotMutateSharedCubeBox(t *testing.T) {
	cb := testSamplerCubeBox(t, cubeboxstore.Status{StartedAt: 1})
	cb.CGroupPath = "/cube_sandbox/sandbox/1"
	store := &fakeGuestWorkloadStore{boxes: []*cubeboxstore.CubeBox{cb}}
	guest := testGuestWorkloadSampler(t, store, &fakeGuestWorkloadReader{})
	host := testHostSandboxSampler(t, store, &fakeHostSandboxReader{})

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, check := range []func() time.Duration{guest.nextDispatchInterval, host.nextDispatchInterval} {
		wg.Add(1)
		go func(check func() time.Duration) {
			defer wg.Done()
			<-start
			for range 1000 {
				check()
			}
		}(check)
	}
	close(start)
	wg.Wait()

	require.Nil(t, cb.Status)
}

func TestGuestWorkloadSamplerListsSandboxesOncePerCollectionCycle(t *testing.T) {
	boxes := []*cubeboxstore.CubeBox{
		testSamplerCubeBoxWithID(t, "b", cubeboxstore.Status{StartedAt: 1}),
		testSamplerCubeBoxWithID(t, "a", cubeboxstore.Status{StartedAt: 1}),
	}
	store := &fakeGuestWorkloadStore{boxes: boxes}
	release := make(chan struct{}, len(boxes))
	metrics := make([]*types.Metric, len(boxes))
	for i, id := range []string{"a", "b"} {
		metrics[i] = testContainerdMetric(t, time.Unix(int64(10+i), 0), testGuestValues{memoryLimit: 16384})
		metrics[i].ID = id
	}
	reader := &fakeGuestWorkloadReader{
		metrics: metrics,
		started: make(chan struct{}, len(boxes)),
		release: release,
	}
	sampler := testGuestWorkloadSampler(t, store, reader)

	require.True(t, sampler.CollectOnce(context.Background()))
	<-reader.started
	require.Equal(t, int64(1), store.listCount())
	require.Equal(t, 500*time.Millisecond, sampler.nextDispatchInterval())
	require.Equal(t, int64(1), store.listCount())

	release <- struct{}{}
	waitForGuestSamplerIdle(t, sampler)
	require.False(t, sampler.CollectOnce(context.Background()))
	<-reader.started
	require.Equal(t, int64(1), store.listCount())
	release <- struct{}{}
	waitForGuestSamplerIdle(t, sampler)

	require.Equal(t, 500*time.Millisecond, sampler.nextDispatchInterval())
	require.Equal(t, int64(1), store.listCount())
	store.boxes = nil
	require.False(t, sampler.CollectOnce(context.Background()))
	require.Equal(t, int64(2), store.listCount())
}

func TestHostSandboxSamplerListsSandboxesOncePerCollectionCycle(t *testing.T) {
	boxes := make([]*cubeboxstore.CubeBox, 2)
	for i, id := range []string{"b", "a"} {
		boxes[i] = testSamplerCubeBoxWithID(t, id, cubeboxstore.Status{StartedAt: 1})
		boxes[i].CGroupPath = fmt.Sprintf("/cube_sandbox/sandbox/%s", id)
	}
	store := &fakeGuestWorkloadStore{boxes: boxes}
	release := make(chan struct{}, len(boxes))
	reader := &fakeHostSandboxReader{
		started: make(chan struct{}, len(boxes)),
		release: release,
	}
	sampler := testHostSandboxSampler(t, store, reader)

	require.True(t, sampler.CollectOnce(context.Background()))
	<-reader.started
	require.Equal(t, int64(1), store.listCount())
	require.Equal(t, 500*time.Millisecond, sampler.nextDispatchInterval())
	require.Equal(t, int64(1), store.listCount())

	release <- struct{}{}
	waitForHostSamplerIdle(t, sampler)
	require.False(t, sampler.CollectOnce(context.Background()))
	<-reader.started
	require.Equal(t, int64(1), store.listCount())
	release <- struct{}{}
	waitForHostSamplerIdle(t, sampler)

	require.Equal(t, 500*time.Millisecond, sampler.nextDispatchInterval())
	require.Equal(t, int64(1), store.listCount())
	store.boxes = nil
	require.False(t, sampler.CollectOnce(context.Background()))
	require.Equal(t, int64(2), store.listCount())
}

func waitForGuestSamplerIdle(t *testing.T, sampler *GuestWorkloadSampler) {
	t.Helper()
	require.Eventually(t, func() bool {
		sampler.mu.RLock()
		defer sampler.mu.RUnlock()
		return len(sampler.inFlight) == 0
	}, time.Second, time.Millisecond)
}

func waitForHostSamplerIdle(t *testing.T, sampler *HostSandboxSampler) {
	t.Helper()
	require.Eventually(t, func() bool {
		sampler.mu.RLock()
		defer sampler.mu.RUnlock()
		return len(sampler.inFlight) == 0
	}, time.Second, time.Millisecond)
}

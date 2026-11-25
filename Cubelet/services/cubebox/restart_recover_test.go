// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/restartpolicy"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

type memBoxSet struct {
	mu    sync.Mutex
	byID  map[string]*cubeboxstore.CubeBox
	saved []*cubeboxstore.CubeBox
	locks map[string]*sync.Mutex
}

func newMemBoxSet(boxes ...*cubeboxstore.CubeBox) *memBoxSet {
	s := &memBoxSet{byID: map[string]*cubeboxstore.CubeBox{}, locks: map[string]*sync.Mutex{}}
	for _, cb := range boxes {
		s.byID[cb.ID] = cb
		s.locks[cb.ID] = &sync.Mutex{}
	}
	return s
}

func (s *memBoxSet) Init(context.Context) error { return nil }

func (s *memBoxSet) Get(_ context.Context, id string) (*cubeboxstore.CubeBox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cb, ok := s.byID[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return cb, nil
}

func (s *memBoxSet) FindContainerOfCubebox(context.Context, string) (*cubeboxstore.Container, *cubeboxstore.CubeBox, error) {
	return nil, nil, nil
}

func (s *memBoxSet) List() []*cubeboxstore.CubeBox {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*cubeboxstore.CubeBox, 0, len(s.byID))
	for _, cb := range s.byID {
		out = append(out, cb)
	}
	return out
}

func (s *memBoxSet) IsImageInUse(string) (bool, error) { return false, nil }

func (s *memBoxSet) Save(_ context.Context, cb *cubeboxstore.CubeBox, _ ...cubes.UpdateCubeboxOpt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, cb.DeepCopy())
	return nil
}

func (s *memBoxSet) SyncByID(context.Context, string, ...cubes.UpdateCubeboxOpt) error { return nil }

func (s *memBoxSet) Delete(context.Context, *cubes.DeleteOption) error { return nil }

func (s *memBoxSet) snapshots() []*cubeboxstore.CubeBox {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*cubeboxstore.CubeBox(nil), s.saved...)
}

func restartBox(t *testing.T, id string, policy string, state string, status cubeboxstore.Status) *cubeboxstore.CubeBox {
	t.Helper()
	cb := &cubeboxstore.CubeBox{ContainersMap: &cubeboxstore.ContainersMap{}}
	cb.ID = id
	cb.RestartState = state
	ctr := &cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{ID: id},
		IsPod:    true,
		Status:   cubeboxstore.StoreStatus(status),
	}
	cb.AddContainer(ctr)
	if err := cb.SetOriginalRequest(&cubebox.RunCubeSandboxRequest{
		RestartPolicy:  policy,
		RestartBackoff: &cubebox.RestartBackoffConfig{InitialIntervalSecond: 1, Multiplier: 1},
	}); err != nil {
		t.Fatal(err)
	}
	return cb
}

func TestRecoveredExit(t *testing.T) {
	started := time.Now().Add(-time.Hour).UnixNano()
	for _, tc := range []struct {
		name string
		cb   *cubeboxstore.CubeBox
		want restartpolicy.Reason
		code int32
	}{
		{
			name: "first run, task gone",
			cb:   restartBox(t, "sb", restartpolicy.OnFailure, restartpolicy.StateRunning, cubeboxstore.Status{StartedAt: started, FinishedAt: started + 1, Unknown: true}),
			want: restartpolicy.ReasonSandboxCrashed,
		},
		{
			name: "first run, container gone",
			cb:   restartBox(t, "sb", restartpolicy.OnFailure, restartpolicy.StateRunning, cubeboxstore.Status{StartedAt: started, FinishedAt: started + 1, ExitUnobserved: true}),
			want: restartpolicy.ReasonSandboxCrashed,
		},
		{
			name: "restarted before, host crashed",
			cb: func() *cubeboxstore.CubeBox {
				cb := restartBox(t, "sb", restartpolicy.OnFailure, restartpolicy.StateRunning, cubeboxstore.Status{StartedAt: started, FinishedAt: started + 1, ExitUnobserved: true})
				cb.LastExitCode = 1
				cb.LastExitReason = string(restartpolicy.ReasonError)
				return cb
			}(),
			want: restartpolicy.ReasonSandboxCrashed,
		},
		{
			name: "clean exit while cubelet was down, stale error from before",
			cb: func() *cubeboxstore.CubeBox {
				cb := restartBox(t, "sb", restartpolicy.OnFailure, restartpolicy.StateRunning, cubeboxstore.Status{
					StartedAt: started, FinishedAt: started + 1, Reason: string(restartpolicy.ReasonCompleted),
				})
				cb.LastExitCode = 1
				cb.LastExitReason = string(restartpolicy.ReasonError)
				return cb
			}(),
			want: restartpolicy.ReasonCompleted,
		},
		{
			name: "failure exit while cubelet was down, stored Completed",
			cb: func() *cubeboxstore.CubeBox {
				cb := restartBox(t, "sb", restartpolicy.OnFailure, restartpolicy.StateRunning, cubeboxstore.Status{
					StartedAt: started, FinishedAt: started + 1, ExitCode: 42, Reason: string(restartpolicy.ReasonError),
				})
				cb.LastExitCode = 0
				cb.LastExitReason = string(restartpolicy.ReasonCompleted)
				return cb
			}(),
			want: restartpolicy.ReasonError,
			code: 42,
		},
		{
			name: "backoff after liveness failure",
			cb: func() *cubeboxstore.CubeBox {
				cb := restartBox(t, "sb", restartpolicy.OnFailure, restartpolicy.StateBackOff, cubeboxstore.Status{StartedAt: started, FinishedAt: started + 1, Unknown: true})
				cb.LastExitReason = string(restartpolicy.ReasonLivenessFailed)
				return cb
			}(),
			want: restartpolicy.ReasonLivenessFailed,
		},
		{
			name: "restarting after oom",
			cb: func() *cubeboxstore.CubeBox {
				cb := restartBox(t, "sb", restartpolicy.OnFailure, restartpolicy.StateRestarting, cubeboxstore.Status{StartedAt: started, FinishedAt: started + 1, ExitCode: 137, Reason: string(restartpolicy.ReasonOOMKilled)})
				cb.LastExitCode = 137
				cb.LastExitReason = string(restartpolicy.ReasonOOMKilled)
				return cb
			}(),
			want: restartpolicy.ReasonOOMKilled,
			code: 137,
		},
		{
			name: "row with no containers and no reason",
			cb:   &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sb"}, ContainersMap: &cubeboxstore.ContainersMap{}},
			want: restartpolicy.ReasonSandboxCrashed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, got := recoveredExit(tc.cb)
			if got != tc.want || code != tc.code {
				t.Fatalf("recoveredExit = (%d, %s), want (%d, %s)", code, got, tc.code, tc.want)
			}
		})
	}
}

func TestRecoverSkipsTerminalAndDeletedSandboxes(t *testing.T) {
	now := time.Now()
	deletedAt := now
	for _, tc := range []struct {
		name   string
		state  string
		delete bool
	}{
		{name: "gave up", state: restartpolicy.StateGaveUp},
		{name: "succeeded", state: restartpolicy.StateSucceeded},
		{name: "failed", state: restartpolicy.StateFailed},
		{name: "being deleted", state: restartpolicy.StateRunning, delete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cb := restartBox(t, "sb", restartpolicy.OnFailure, tc.state, cubeboxstore.Status{
				StartedAt: now.Add(-time.Hour).UnixNano(), FinishedAt: now.Add(-time.Minute).UnixNano(),
			})
			cb.StatusSeq = 5
			if tc.delete {
				cb.UserMarkDeletedTime = &deletedAt
			}
			store := newMemBoxSet(cb)
			m := newRestartMgr(store)
			m.Recover()
			m.Stop("sb")

			if cb.StatusSeq != 5 || cb.RestartState != tc.state {
				t.Fatalf("recovery rewrote a terminal row: seq=%d state=%s", cb.StatusSeq, cb.RestartState)
			}
			if _, ok := m.runs["sb"]; ok {
				t.Fatal("recovery armed a restart for a row it must leave alone")
			}
			if n := len(store.snapshots()); n != 0 {
				t.Fatalf("recovery saved %d times for a row it must leave alone", n)
			}
		})
	}
}

func TestRecoverDoesNotBackfillAnchorForStoppedSandbox(t *testing.T) {
	now := time.Now()
	cb := restartBox(t, "sb", restartpolicy.OnFailure, restartpolicy.StateRunning, cubeboxstore.Status{
		StartedAt: now.Add(-time.Hour).UnixNano(), FinishedAt: now.UnixNano(), Unknown: true,
	})
	cb.StableSince = 0
	store := newMemBoxSet(cb)
	m := newRestartMgr(store)

	m.Recover()
	t.Cleanup(func() { m.Stop("sb") })

	deadline := time.Now().Add(2 * time.Second)
	for {
		snaps := store.snapshots()
		for _, s := range snaps {
			if s.StableSince != 0 {
				t.Fatalf("recovery backfilled a stability anchor for a stopped sandbox: %+v", s)
			}
		}
		cb.Lock()
		reason := cb.LastExitReason
		cb.Unlock()
		if reason == string(restartpolicy.ReasonSandboxCrashed) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("host crash was not recorded as a crash: reason=%q state=%s", reason, cb.RestartState)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRecoverRestartsSandboxWithUnobservedExit(t *testing.T) {
	now := time.Now()
	cb := restartBox(t, "sb", restartpolicy.OnFailure, restartpolicy.StateRunning, cubeboxstore.Status{
		StartedAt: now.Add(-time.Hour).UnixNano(), FinishedAt: now.UnixNano(), ExitUnobserved: true,
	})
	m := newRestartMgr(newMemBoxSet(cb))

	m.Recover()
	t.Cleanup(func() { m.Stop("sb") })

	deadline := time.Now().Add(2 * time.Second)
	for {
		cb.Lock()
		state, reason := cb.RestartState, cb.LastExitReason
		cb.Unlock()
		if reason == string(restartpolicy.ReasonSandboxCrashed) {
			if state == restartpolicy.StateSucceeded {
				t.Fatalf("a crash was finalized as %s", state)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("unobserved exit was not recorded: reason=%q state=%s", reason, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRecoverKeepsCleanExitTerminal(t *testing.T) {
	now := time.Now()
	cb := restartBox(t, "sb", restartpolicy.OnFailure, restartpolicy.StateRunning, cubeboxstore.Status{
		StartedAt: now.Add(-time.Hour).UnixNano(), FinishedAt: now.UnixNano(), Reason: string(restartpolicy.ReasonCompleted),
	})
	cb.LastExitCode = 1
	cb.LastExitReason = string(restartpolicy.ReasonError)
	m := newRestartMgr(newMemBoxSet(cb))

	m.Recover()
	t.Cleanup(func() { m.Stop("sb") })

	deadline := time.Now().Add(2 * time.Second)
	for {
		cb.Lock()
		state, count := cb.RestartState, cb.RestartCount
		cb.Unlock()
		if state == restartpolicy.StateSucceeded {
			if count != 0 {
				t.Fatalf("a clean exit consumed a restart: count=%d", count)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("clean exit did not settle as Succeeded: state=%s count=%d", state, count)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type blockingRestarter struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	calls    int
	release  chan struct{}
	class    restartClass
	code     errorcode.ErrorCode
}

func (b *blockingRestarter) restartClassified(_ context.Context, req *cubebox.RunCubeSandboxRequest) (*cubebox.RunCubeSandboxResponse, restartClass, error) {
	b.mu.Lock()
	b.inFlight++
	b.calls++
	if b.inFlight > b.peak {
		b.peak = b.inFlight
	}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.inFlight--
		b.mu.Unlock()
	}()
	if b.release != nil {
		<-b.release
	}
	return &cubebox.RunCubeSandboxResponse{
		SandboxID: req.GetAnnotations()["cube.master.desired.sandbox.id"],
		Ret:       &errorcode.Ret{RetCode: b.code, RetMsg: "test"},
	}, b.class, nil
}

func (b *blockingRestarter) stats() (calls, peak int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls, b.peak
}

func exitAll(m *restartMgr, ids []string) {
	for _, id := range ids {
		m.OnExit(id, 1, restartpolicy.ReasonError)
	}
}

// armRestartRun registers a run with a custom backoff, bypassing proto defaults.
func armRestartRun(m *restartMgr, cb *cubeboxstore.CubeBox, backoff restartpolicy.Backoff) {
	req, err := cb.CopyOriginalRequest()
	if err != nil || req == nil {
		panic("armRestartRun: sandbox has no stored original request")
	}
	m.mu.Lock()
	m.runs[cb.ID] = &restartRun{req: req, backoff: backoff}
	m.mu.Unlock()
}

func fastBackoff() restartpolicy.Backoff {
	return restartpolicy.Backoff{Initial: time.Millisecond, Max: time.Millisecond, Multiplier: 1, MaxRestarts: 5}
}

func TestRestartLoopBoundsConcurrency(t *testing.T) {
	const sandboxes = 12
	boxes := make([]*cubeboxstore.CubeBox, 0, sandboxes)
	ids := make([]string, 0, sandboxes)
	for i := 0; i < sandboxes; i++ {
		id := "sb-" + string(rune('a'+i))
		cb := restartBox(t, id, restartpolicy.OnFailure, restartpolicy.StateRunning, cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
		cb.LastExitReason = string(restartpolicy.ReasonError)
		boxes = append(boxes, cb)
		ids = append(ids, id)
	}
	m := newRestartMgr(newMemBoxSet(boxes...))
	m.slots = make(chan struct{}, 3)
	restarter := &blockingRestarter{release: make(chan struct{}), class: restartOK}
	m.restarter = restarter

	for _, cb := range boxes {
		armRestartRun(m, cb, fastBackoff())
	}
	exitAll(m, ids)
	t.Cleanup(func() { m.Stop("") })

	deadline := time.Now().Add(2 * time.Second)
	for {
		calls, peak := restarter.stats()
		if calls >= 3 {
			if peak > 3 {
				t.Fatalf("%d rebuilds ran at once with 3 slots", peak)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d rebuilds started", calls)
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if calls, _ := restarter.stats(); calls > 3 {
		t.Fatalf("%d rebuilds started while every slot was taken", calls)
	}

	close(restarter.release)
	for _, id := range ids {
		t.Cleanup(func() { m.Stop(id) })
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		calls, peak := restarter.stats()
		if calls >= sandboxes {
			if peak > 3 {
				t.Fatalf("%d rebuilds ran at once with 3 slots", peak)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue did not drain: %d/%d rebuilds", calls, sandboxes)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRestartQueuedForSlotStopsImmediately(t *testing.T) {
	cb := restartBox(t, "sb", restartpolicy.OnFailure, restartpolicy.StateRunning, cubeboxstore.Status{StartedAt: time.Now().UnixNano()})
	m := newRestartMgr(newMemBoxSet(cb))
	m.slots = make(chan struct{}, 1)
	m.slots <- struct{}{} // occupy the only slot
	restarter := &blockingRestarter{class: restartOK}
	m.restarter = restarter

	armRestartRun(m, cb, fastBackoff())
	m.OnExit("sb", 1, restartpolicy.ReasonError)
	time.Sleep(100 * time.Millisecond)
	if calls, _ := restarter.stats(); calls != 0 {
		t.Fatalf("%d rebuilds started without a slot", calls)
	}

	m.Stop("sb")
	deadline := time.Now().Add(2 * time.Second)
	for {
		m.mu.Lock()
		_, tracked := m.runs["sb"]
		m.mu.Unlock()
		if !tracked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("stopped sandbox stayed in the restart set")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLockBusyDoesNotSpendRetryBudgets(t *testing.T) {
	cb := restartBox(t, "sb", restartpolicy.OnFailure, restartpolicy.StateBackOff, cubeboxstore.Status{
		StartedAt: time.Now().UnixNano(), FinishedAt: time.Now().UnixNano(),
	})
	cb.LastExitReason = string(restartpolicy.ReasonError)
	cb.DestroyFailCount = 1
	m := newRestartMgr(newMemBoxSet(cb))
	restarter := &blockingRestarter{class: restartLockBusy}
	m.restarter = restarter

	armRestartRun(m, cb, fastBackoff())
	m.OnExit("sb", 1, restartpolicy.ReasonError)

	deadline := time.Now().Add(2 * time.Second)
	for {
		calls, _ := restarter.stats()
		if calls >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock-busy attempts were not retried: %d calls", calls)
		}
		time.Sleep(5 * time.Millisecond)
	}
	m.Stop("sb")

	cb.Lock()
	count, destroyFails, state := cb.RestartCount, cb.DestroyFailCount, cb.RestartState
	cb.Unlock()
	if count != 0 || destroyFails != 1 {
		t.Fatalf("node-limit rejection spent a budget: restartCount=%d destroyFails=%d", count, destroyFails)
	}
	if state == restartpolicy.StateGaveUp {
		t.Fatal("node-limit rejection gave the sandbox up")
	}
}

func TestIsConcurrencyRejection(t *testing.T) {
	if !isConcurrencyRejection(errorcode.ErrorCode_ConcurrentFailed) {
		t.Fatal("the workflow concurrency code was not recognised")
	}
	if isConcurrencyRejection(errorcode.ErrorCode_Success) {
		t.Fatal("a successful code was treated as a rejection")
	}
}

func TestConcurrencyCodeSurvivesRetConversion(t *testing.T) {
	err := ret.Errorf(errorcode.ErrorCode_ConcurrentFailed, "flow [create] exceed limited")
	rerr, ok := ret.FromError(err)
	if !ok || !isConcurrencyRejection(rerr.Code()) {
		t.Fatalf("converted code %v ok=%v", rerr, ok)
	}
}

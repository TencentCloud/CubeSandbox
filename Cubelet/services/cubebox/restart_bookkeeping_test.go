// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/restartpolicy"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func TestRestartRequestTraceIsOnTheContext(t *testing.T) {
	req := &cubebox.RunCubeSandboxRequest{RequestID: "req-1"}
	rt := restartRequestTrace(req, "sb-1", "workflow", "Destroy")
	ctx := CubeLog.WithRequestTrace(context.Background(), rt)
	got := CubeLog.GetTraceInfo(ctx)
	if got == nil || got.Action != "Restart" || got.RequestID != "req-1" || got.InstanceID != "sb-1" || got.CalleeAction != "Destroy" {
		t.Fatalf("trace %+v", got)
	}
	rt.CalleeAction = "Create"
	ctx = CubeLog.WithRequestTrace(ctx, rt)
	got = CubeLog.GetTraceInfo(ctx)
	if got == nil || got.CalleeAction != "Create" || got.InstanceID != "sb-1" {
		t.Fatalf("create trace %+v", got)
	}
}

func TestApplyRestartBookkeepingKeepsSeq(t *testing.T) {
	prior := &workflow.RestartPrior{
		RestartCount: 3, StatusSeq: 8, LastExitCode: 1, LastExitReason: "Error",
	}
	restarted := &cubeboxstore.CubeBox{}
	applyRestartBookkeeping(restarted, prior, true)
	if restarted.StatusSeq != 8 || restarted.RestartCount != 4 || restarted.RestartState != restartpolicy.StateRestarting {
		t.Fatalf("restart seq=%d count=%d state=%s", restarted.StatusSeq, restarted.RestartCount, restarted.RestartState)
	}
	if restarted.LastExitReason != "Error" {
		t.Fatalf("exit reason %q", restarted.LastExitReason)
	}

	resumed := &cubeboxstore.CubeBox{}
	applyRestartBookkeeping(resumed, prior, false)
	if resumed.StatusSeq != 8 || resumed.RestartCount != 3 || resumed.RestartState != restartpolicy.StateRunning {
		t.Fatalf("resume seq=%d count=%d state=%s", resumed.StatusSeq, resumed.RestartCount, resumed.RestartState)
	}
}

func TestOnExitCompletedStopsOnFailureProbes(t *testing.T) {
	cb := &cubeboxstore.CubeBox{ContainersMap: &cubeboxstore.ContainersMap{}}
	cb.ID = "sb"
	m := newRestartMgr(&memBoxes{cb: cb})
	_, cancel := context.WithCancel(context.Background())
	m.runs["sb"] = &restartRun{
		cancel:  cancel,
		req:     &cubebox.RunCubeSandboxRequest{RestartPolicy: restartpolicy.OnFailure},
		backoff: restartpolicy.DefaultBackoff(),
	}
	m.OnExit("sb", 0, restartpolicy.ReasonCompleted)
	m.mu.Lock()
	_, ok := m.runs["sb"]
	m.mu.Unlock()
	if ok {
		t.Fatal("completed OnFailure left the probe run in place")
	}
	if cb.RestartState != restartpolicy.StateSucceeded || cb.LastExitCode != 0 || cb.LastExitReason != string(restartpolicy.ReasonCompleted) || cb.RestartCount != 0 {
		t.Fatalf("state=%s code=%d reason=%s count=%d", cb.RestartState, cb.LastExitCode, cb.LastExitReason, cb.RestartCount)
	}
}

func TestOnExitWithoutRunRecordsTerminal(t *testing.T) {
	cb := &cubeboxstore.CubeBox{ContainersMap: &cubeboxstore.ContainersMap{}}
	cb.ID = "sb"
	m := newRestartMgr(&memBoxes{cb: cb})
	m.OnExit("sb", 137, restartpolicy.ReasonError)
	if cb.RestartState != restartpolicy.StateFailed || cb.LastExitCode != 137 || cb.RestartCount != 0 {
		t.Fatalf("state=%s code=%d count=%d", cb.RestartState, cb.LastExitCode, cb.RestartCount)
	}
}

func TestOnExitSkipsTerminalWhileRestarting(t *testing.T) {
	cb := &cubeboxstore.CubeBox{ContainersMap: &cubeboxstore.ContainersMap{}}
	cb.ID = "sb"
	cb.RestartState = restartpolicy.StateBackOff
	m := newRestartMgr(&memBoxes{cb: cb})
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.runs["sb"] = &restartRun{cancel: cancel, restarting: true}
	m.OnExit("sb", 0, restartpolicy.ReasonCompleted)
	if cb.RestartState != restartpolicy.StateBackOff || cb.LastExitReason != "" {
		t.Fatalf("state=%s reason=%s", cb.RestartState, cb.LastExitReason)
	}
}

func TestClearRestartCountForResume(t *testing.T) {
	box := &cubeboxstore.CubeBox{}
	box.RestartCount = 2
	box.RestartState = restartpolicy.StateBackOff
	clearRestartCountForResume(box)
	if box.RestartCount != 0 || box.RestartState != restartpolicy.StateRunning || box.StatusSeq != 1 {
		t.Fatalf("count=%d state=%s seq=%d", box.RestartCount, box.RestartState, box.StatusSeq)
	}
	clearRestartCountForResume(box)
	if box.StatusSeq != 1 {
		t.Fatalf("unchanged resume bumped seq to %d", box.StatusSeq)
	}
	clearRestartCountForResume(nil)
}

func TestRecoverGaveUpWhenRetriesSpent(t *testing.T) {
	cb := &cubeboxstore.CubeBox{ContainersMap: &cubeboxstore.ContainersMap{}}
	cb.ID = "sb"
	cb.RestartCount = 2
	cb.LastExitReason = "Error"
	cb.RestartState = restartpolicy.StateBackOff
	if err := cb.SetOriginalRequest(&cubebox.RunCubeSandboxRequest{
		RestartPolicy:  restartpolicy.OnFailure,
		RestartBackoff: &cubebox.RestartBackoffConfig{MaxRestarts: 2},
	}); err != nil {
		t.Fatal(err)
	}
	m := newRestartMgr(&memBoxes{cb: cb})
	m.Recover()
	if cb.RestartState != restartpolicy.StateGaveUp {
		t.Fatalf("state %s", cb.RestartState)
	}
	if cb.StatusSeq == 0 {
		t.Fatal("gave up did not advance status seq")
	}
}

type memBoxes struct {
	cb *cubeboxstore.CubeBox
}

func (m *memBoxes) Init(context.Context) error { return nil }
func (m *memBoxes) Get(context.Context, string) (*cubeboxstore.CubeBox, error) {
	return m.cb, nil
}
func (m *memBoxes) FindContainerOfCubebox(context.Context, string) (*cubeboxstore.Container, *cubeboxstore.CubeBox, error) {
	return nil, nil, nil
}
func (m *memBoxes) List() []*cubeboxstore.CubeBox { return []*cubeboxstore.CubeBox{m.cb} }
func (m *memBoxes) IsImageInUse(string) (bool, error) {
	return false, nil
}
func (m *memBoxes) Save(context.Context, *cubeboxstore.CubeBox, ...cubes.UpdateCubeboxOpt) error {
	return nil
}
func (m *memBoxes) SyncByID(context.Context, string, ...cubes.UpdateCubeboxOpt) error {
	return nil
}
func (m *memBoxes) Delete(context.Context, *cubes.DeleteOption) error { return nil }

func TestCreateWindowExitZeroStaysDown(t *testing.T) {
	cb := &cubeboxstore.CubeBox{ContainersMap: &cubeboxstore.ContainersMap{}}
	cb.ID = "sb"
	m := newRestartMgr(&memBoxes{cb: cb})
	m.armCreate("sb", &cubebox.RunCubeSandboxRequest{RestartPolicy: restartpolicy.OnFailure})
	if run := m.runs["sb"]; run == nil || !run.pending {
		t.Fatal("expected a pending run without probes")
	}
	m.OnExit("sb", 0, restartpolicy.ReasonCompleted)
	if cb.RestartState == restartpolicy.StateSucceeded {
		t.Fatal("exit during create was finalized")
	}
	m.finishCreate("sb", true)
	if cb.RestartState != restartpolicy.StateSucceeded || cb.LastExitReason != string(restartpolicy.ReasonCompleted) {
		t.Fatalf("state=%s reason=%s", cb.RestartState, cb.LastExitReason)
	}
}

func TestCreateWindowQueuesNonZero(t *testing.T) {
	cb := &cubeboxstore.CubeBox{ContainersMap: &cubeboxstore.ContainersMap{}}
	cb.ID = "sb"
	m := newRestartMgr(&memBoxes{cb: cb})
	m.armCreate("sb", &cubebox.RunCubeSandboxRequest{RestartPolicy: restartpolicy.OnFailure})
	m.OnExit("sb", 7, restartpolicy.ReasonError)
	run := m.runs["sb"]
	if run == nil || run.queued == nil || run.queued.code != 7 {
		t.Fatal("non-zero exit was not queued")
	}
	if cb.RestartState == restartpolicy.StateFailed {
		t.Fatal("queued exit was finalized")
	}
}

func TestAbandonedCreateIgnoresLateExit(t *testing.T) {
	cb := &cubeboxstore.CubeBox{ContainersMap: &cubeboxstore.ContainersMap{}}
	cb.ID = "sb"
	cb.RestartState = restartpolicy.StateRunning
	m := newRestartMgr(&memBoxes{cb: cb})
	m.armCreate("sb", &cubebox.RunCubeSandboxRequest{RestartPolicy: restartpolicy.OnFailure})
	m.abandonCreate("sb")
	m.OnExit("sb", 1, restartpolicy.ReasonError)
	if cb.RestartState != restartpolicy.StateRunning {
		t.Fatalf("late exit changed state to %s", cb.RestartState)
	}
}

func TestRestartingCreateQueuesExit(t *testing.T) {
	cb := &cubeboxstore.CubeBox{ContainersMap: &cubeboxstore.ContainersMap{}}
	cb.ID = "sb"
	m := newRestartMgr(&memBoxes{cb: cb})
	m.runs["sb"] = &restartRun{restarting: true}
	m.OnExit("sb", 0, restartpolicy.ReasonCompleted)
	if m.runs["sb"].queued == nil {
		t.Fatal("exit during restart create was dropped")
	}
	if cb.LastExitReason != "" {
		t.Fatalf("queued exit was recorded as %s", cb.LastExitReason)
	}
}

func TestDestroySIGKILLIsNotANewExit(t *testing.T) {
	m := newRestartMgr(&memBoxes{})
	m.runs["sb"] = &restartRun{restarting: true}
	m.noteDestroyPID("sb", 42)
	if !m.swallowRestartDestroyExit("sb", 42, 42) {
		t.Fatal("exit of the pid destroy is killing was not swallowed")
	}
	if !m.swallowRestartDestroyExit("sb", 0, 42) {
		t.Fatal("pid 0 during restart destroy was not swallowed")
	}
	if m.swallowRestartDestroyExit("sb", 99, 99) {
		t.Fatal("new pid was swallowed")
	}
	m.OnExit("sb", 1, restartpolicy.ReasonError)
	if m.runs["sb"].queued == nil || m.runs["sb"].queued.code != 1 {
		t.Fatal("create-window exit was not queued")
	}
	m.clearDestroySuppress("sb")
	if m.swallowRestartDestroyExit("sb", 42, 99) {
		t.Fatal("suppress leaked after the cold start was accepted")
	}
}

func TestDestroyWithNoShimPIDSwallowsUntilNewPID(t *testing.T) {
	m := newRestartMgr(&memBoxes{})
	m.runs["sb"] = &restartRun{restarting: true}
	m.noteDestroyPID("sb", 0)
	if !m.swallowRestartDestroyExit("sb", 9, 0) {
		t.Fatal("exit 9 while stored pid is still 0 was not swallowed")
	}
	if !m.swallowRestartDestroyExit("sb", 0, 77) {
		t.Fatal("pid 0 after the new pid was published was not swallowed")
	}
	if m.swallowRestartDestroyExit("sb", 77, 77) {
		t.Fatal("new pid exit was swallowed")
	}
}

func TestRestartPinsTheAnnotatedNumaNode(t *testing.T) {
	restoreNumaQueries(t, 2, 2)
	s := &service{numaNodeIndex: 1}
	info := &workflow.CreateContext{}
	req := &cubebox.RunCubeSandboxRequest{
		Annotations: map[string]string{constants.MasterAnnotationsNumaNode: "1"},
	}
	if err := s.setRequestResource(info, req); err != nil {
		t.Fatal(err)
	}
	if info.NumaNode != 1 {
		t.Fatalf("numa node %d", info.NumaNode)
	}
}

func TestRestartRoundRobinsWhenNumaIsUnspecified(t *testing.T) {
	restoreNumaQueries(t, 2, 2)
	s := &service{}
	info := &workflow.CreateContext{}
	if err := s.setRequestResource(info, &cubebox.RunCubeSandboxRequest{}); err != nil {
		t.Fatal(err)
	}
	if info.NumaNode != 1 {
		t.Fatalf("numa node %d, want the next round-robin slot", info.NumaNode)
	}
}

func restoreNumaQueries(t *testing.T, maxID, count int) {
	t.Helper()
	prevMax, prevCount := numaMaxNodeID, numaNodeCount
	numaMaxNodeID = func() int { return maxID }
	numaNodeCount = func() int { return count }
	t.Cleanup(func() {
		numaMaxNodeID = prevMax
		numaNodeCount = prevCount
	})
}

func TestSavedContainerIDsKeepRequestIndexWhenCreatedAtTies(t *testing.T) {
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sb"}}
	cb.AddContainer(&cubeboxstore.Container{
		Metadata:        cubeboxstore.Metadata{ID: "sb", CreatedAt: 100},
		IsPod:           true,
		RequestIndex:    0,
		RequestIndexSet: true,
	})
	cb.AddContainer(&cubeboxstore.Container{
		Metadata:        cubeboxstore.Metadata{ID: "side-b", CreatedAt: 100},
		RequestIndex:    2,
		RequestIndexSet: true,
	})
	cb.AddContainer(&cubeboxstore.Container{
		Metadata:        cubeboxstore.Metadata{ID: "side-a", CreatedAt: 100},
		RequestIndex:    1,
		RequestIndexSet: true,
	})
	got := savedContainerIDs(cb)
	if got[0] != "sb" || got[1] != "side-a" || got[2] != "side-b" {
		t.Fatalf("ids %v", got)
	}
}

func TestSavedContainerIDsStableSortForOldRows(t *testing.T) {
	cb := &cubeboxstore.CubeBox{Metadata: cubeboxstore.Metadata{ID: "sb"}}
	cb.AddContainer(&cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{ID: "sb"},
		IsPod:    true,
	})
	cb.AddContainer(&cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{ID: "side-b", CreatedAt: 50},
	})
	cb.AddContainer(&cubeboxstore.Container{
		Metadata: cubeboxstore.Metadata{ID: "side-a", CreatedAt: 50},
	})
	got := savedContainerIDs(cb)
	if got[1] != "side-a" || got[2] != "side-b" {
		t.Fatalf("ids %v", got)
	}
}

func TestRequestIndexZeroIsDistinctFromAnOldRow(t *testing.T) {
	raw, err := json.Marshal(&cubeboxstore.Container{
		Metadata:        cubeboxstore.Metadata{ID: "sb"},
		RequestIndex:    0,
		RequestIndexSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var restored cubeboxstore.Container
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.RequestIndex != 0 || !restored.RequestIndexSet {
		t.Fatalf("index 0 was not stored: %+v", restored)
	}
	var old cubeboxstore.Container
	if err := json.Unmarshal([]byte(`{"ID":"old"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.RequestIndexSet {
		t.Fatal("an old row was treated as request index 0")
	}
}

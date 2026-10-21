// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package workflow

import (
	"context"
	"fmt"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	cubesem "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

func TestLimiter(t *testing.T) {
	runcc := int64(4)
	limiter := semaphore.NewWeighted(runcc)
	assert.NotNil(t, limiter)
	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	wg.Add(int(runcc))
	job := func(i int) error {
		if err := limiter.Acquire(context.Background(), 1); err != nil {
			wg.Done()
			return err
		}
		defer limiter.Release(1)
		wg.Done()
		log.Printf("running %d", i)
		defer log.Printf("end %d", i)
		for {
			select {
			case <-ctx.Done():
				return nil
			}
		}
	}

	for i := 0; i < int(runcc); i++ {
		go func(j int) {
			err := job(j)
			if err != nil {
				assert.FailNow(t, "should not be err")
			}
		}(i)
	}
	wg.Wait()

	ctxd, dcancel := context.WithTimeout(context.Background(), time.Second)
	defer dcancel()
	if err := limiter.Acquire(ctxd, 1); err == nil {
		assert.FailNow(t, "should be err")
	}
	assert.False(t, limiter.TryAcquire(1))
	cancel()
	time.Sleep(time.Second)
}

func TestErrgroupWithCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var got error
	unexpected := make(chan struct{})
	go func() {
		eg, ctxWithCancel := errgroup.WithContext(ctx)
		for j := 0; j < 10; j++ {
			i := j
			eg.Go(func() error {
				select {
				case <-ctxWithCancel.Done():
					return ctxWithCancel.Err()
				case <-time.After(5 * time.Second):
					return fmt.Errorf("timeout %d", i)
				}
			})
		}
		got = eg.Wait()
		if got != nil && got != context.Canceled {
			unexpected <- struct{}{}
		}
	}()
	time.AfterFunc(3*time.Second, func() {

		cancel()
	})
	time.Sleep(6 * time.Second)
	select {
	case <-ctx.Done():
		if ctx.Err() != context.Canceled {
			t.Fatalf("ctx.Err() != context.Canceled")
		}
	case <-unexpected:
		t.Fatal("unexpected")
	}
}

type recordingDestroyStep struct {
	name     string
	calls    int
	ctxErr   error
	deadline time.Time
	hasDue   bool
}

func (s *recordingDestroyStep) ID() string { return s.name }

func (s *recordingDestroyStep) Init(context.Context, *InitInfo) error { return nil }

func (s *recordingDestroyStep) Create(context.Context, *CreateContext) error { return nil }

func (s *recordingDestroyStep) CleanUp(context.Context, *CleanContext) error { return nil }

func (s *recordingDestroyStep) Destroy(ctx context.Context, _ *DestroyContext) error {
	s.calls++
	s.ctxErr = ctx.Err()
	s.deadline, s.hasDue = ctx.Deadline()
	return nil
}

func twoStepDestroyEngine(first, second *recordingDestroyStep) *Engine {
	engine := &Engine{}
	engine.AddFlow(flow_destroy, &Workflow{
		Name:    flow_destroy,
		Limiter: cubesem.NewLimiter(1),
		Steps: []*Step{
			{Name: first.ID(), Actions: []Flow{first}},
			{Name: second.ID(), Actions: []Flow{second}},
		},
	})
	return engine
}

func withDestroyTrace(ctx context.Context) context.Context {
	return CubeLog.WithRequestTrace(ctx, &CubeLog.RequestTrace{RequestID: "restart-destroy"})
}

func TestRestartDestroyContinuesStepsAfterDeadline(t *testing.T) {
	first := &recordingDestroyStep{name: "cubebox"}
	second := &recordingDestroyStep{name: "cgroup"}
	engine := twoStepDestroyEngine(first, second)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := engine.Destroy(withDestroyTrace(ctx), &DestroyContext{
		BaseWorkflowInfo: BaseWorkflowInfo{SandboxID: "sb-restart"},
		IsRestartDestroy: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, first.calls)
	assert.ErrorIs(t, first.ctxErr, context.DeadlineExceeded)
	assert.Equal(t, 1, second.calls)
	assert.NoError(t, second.ctxErr)
	require.True(t, second.hasDue)
	assert.True(t, second.deadline.After(time.Now()))
}

func TestRestartDestroyStopsWhenCallerCancels(t *testing.T) {
	first := &recordingDestroyStep{name: "cubebox"}
	second := &recordingDestroyStep{name: "cgroup"}
	engine := twoStepDestroyEngine(first, second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := engine.Destroy(withDestroyTrace(ctx), &DestroyContext{
		BaseWorkflowInfo: BaseWorkflowInfo{SandboxID: "sb-cancel"},
		IsRestartDestroy: true,
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, first.calls)
	assert.Zero(t, second.calls)
}

func TestDestroyStopsAfterDeadlineWhenNotRestart(t *testing.T) {
	first := &recordingDestroyStep{name: "cubebox"}
	second := &recordingDestroyStep{name: "cgroup"}
	engine := twoStepDestroyEngine(first, second)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := engine.Destroy(withDestroyTrace(ctx), &DestroyContext{
		BaseWorkflowInfo: BaseWorkflowInfo{SandboxID: "sb-delete"},
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 1, first.calls)
	assert.Zero(t, second.calls)
}

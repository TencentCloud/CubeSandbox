// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
	"gorm.io/gorm"
)

// Executor owns every in-flight build the process is running. It exists to
// close three gaps that a bare "go build(...)" goroutine leaves open:
//
//   - graceful shutdown: builds used to run on context.Background, completely
//     detached from the app lifecycle, so a normal SIGTERM killed mkfs/pull
//     mid-flight and left every job lying RUNNING until the reconciler timed
//     it out two hours later. The executor gives each build a child of the app
//     context and drains them on Shutdown.
//   - duplicate submit: the same job_id could be accepted twice and two
//     goroutines would interleave their status reports. The executor rejects a
//     second submit for a job already in flight.
//   - unbounded concurrency: with no cap, any caller able to reach the build
//     endpoint could fan out unlimited image pulls + mkfs runs. The executor
//     caps concurrent builds.
type Executor struct {
	// maxConcurrent bounds simultaneous builds. 0 disables the cap (tests).
	maxConcurrent int

	// build is the work to run per job; a field so tests can substitute a fake
	// that blocks on ctx instead of doing real image pulls and mkfs.
	build func(ctx context.Context, jobID string, req *types.CreateTemplateFromImageReq, downloadBaseURL, envdSHA string, envdData []byte) error
	// lookupJob verifies a job is buildable; a field so tests can bypass the DB.
	lookupJob func(ctx context.Context, jobID string) error

	rootCtx    context.Context
	rootCancel context.CancelFunc
	wg         sync.WaitGroup
	sema       chan struct{}

	mu       sync.Mutex
	inFlight map[string]struct{}
}

// SetBuildFunc swaps the per-job build implementation, and SetLookupFunc swaps
// the job-existence check. Both default to the real Build / DB lookup. They
// exist for tests and for deployments that want to dry-run the channel without
// doing image work; production code paths leave them at the defaults.
func (e *Executor) SetBuildFunc(fn func(ctx context.Context, jobID string, req *types.CreateTemplateFromImageReq, downloadBaseURL, envdSHA string, envdData []byte) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.build = fn
}

func (e *Executor) SetLookupFunc(fn func(ctx context.Context, jobID string) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lookupJob = fn
}

// InFlight reports how many jobs are currently building (observability/tests).
func (e *Executor) InFlight() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.inFlight)
}

// Sentinel errors returned by Submit, mapped to HTTP statuses by the handler.
var (
	// ErrBuildJobDuplicate means this job is already being built here.
	ErrBuildJobDuplicate = errors.New("build job already in flight")
	// ErrBuildConcurrencyLimit means the executor is at capacity.
	ErrBuildConcurrencyLimit = errors.New("too many concurrent builds")
	// ErrBuildJobNotFound means no such job (or it is not in a buildable state).
	ErrBuildJobNotFound = errors.New("build job not found or not in a buildable state")
)

// NewExecutor creates an Executor. maxConcurrent <= 0 means "no limit".
func NewExecutor(maxConcurrent int) *Executor {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Executor{
		maxConcurrent: maxConcurrent,
		build:         Build,
		lookupJob:     lookupBuildableJob,
		rootCtx:       ctx,
		rootCancel:    cancel,
		inFlight:      make(map[string]struct{}),
	}
	if maxConcurrent > 0 {
		e.sema = make(chan struct{}, maxConcurrent)
	}
	return e
}

// buildableJob reports whether the job exists and is in a state worth building.
// A submitted job should be PENDING (just persisted by CubeMaster); RUNNING is
// accepted too so a duplicate delivery after a partial retry does not 404.
func buildableJobStatus(status string) bool {
	return status == templatecenter.JobStatusPending || status == templatecenter.JobStatusRunning
}

// lookupBuildableJob verifies the job exists and is buildable, without trusting
// the caller's payload. Submitting against a fabricated or finished job would
// otherwise burn a full image pull + mkfs for nothing.
func lookupBuildableJob(ctx context.Context, jobID string) error {
	db := templatecenter.GetDB()
	if db == nil {
		// No DB handle: skip the existence check rather than reject every build.
		// This keeps TC usable in DB-less test setups; the reconciler already
		// refuses to run in that state, so behaviour stays consistent.
		return nil
	}
	record := &models.TemplateImageJob{}
	err := db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Select("job_id", "status").
		Where("job_id = ?", jobID).
		First(record).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrBuildJobNotFound
		}
		return fmt.Errorf("lookup build job %s: %w", jobID, err)
	}
	if !buildableJobStatus(record.Status) {
		return fmt.Errorf("%w: job %s status is %q", ErrBuildJobNotFound, jobID, record.Status)
	}
	return nil
}

// Submit validates the job and starts its build in the background. It returns
// immediately; the handler answers 200 from the nil return.
//
// The build runs on a child of the executor's root context (NOT the gin
// request context, which dies when the handler returns, and NOT bare
// Background, which nothing can cancel). Shutdown cancels every in-flight
// build and waits for them.
func (e *Executor) Submit(jobID string, req *types.CreateTemplateFromImageReq, downloadBaseURL, envdSHA string, envdData []byte) error {
	if err := e.lookupJob(e.rootCtx, jobID); err != nil {
		return err
	}

	// Reject a duplicate submit for a job already building here. Two goroutines
	// reporting on one job interleave their phase/progress updates and leave the
	// record in a state neither intended.
	e.mu.Lock()
	if _, dup := e.inFlight[jobID]; dup {
		e.mu.Unlock()
		return ErrBuildJobDuplicate
	}
	e.inFlight[jobID] = struct{}{}
	e.mu.Unlock()

	// Acquire a build slot. Non-blocking so the caller gets a clear 429-class
	// answer instead of an unbounded queue.
	if e.sema != nil {
		select {
		case e.sema <- struct{}{}:
		default:
			e.mu.Lock()
			delete(e.inFlight, jobID)
			e.mu.Unlock()
			return ErrBuildConcurrencyLimit
		}
	}

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		if e.sema != nil {
			defer func() { <-e.sema }()
		}
		defer func() {
			e.mu.Lock()
			delete(e.inFlight, jobID)
			e.mu.Unlock()
		}()
		if err := e.build(e.rootCtx, jobID, req, downloadBaseURL, envdSHA, envdData); err != nil {
			log.G(e.rootCtx).Errorf("build template fail: job_id=%s err=%v", jobID, err)
		}
	}()
	return nil
}

// Shutdown cancels every in-flight build and waits for them to unwind. The
// reporter's cancel path is already wired to ctx.Done, so a cancelled build
// still best-effort-reports its terminal state before returning.
func (e *Executor) Shutdown() {
	e.rootCancel()
	e.wg.Wait()
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/checkpoint"
)

// Background sweeps. Once a checkpoint is committed, what it
// superseded in the sandbox's lineage is deleted in the background: the job
// is recorded exported, and the checkpoint can be read back, without
// waiting for the deletes. A node runs at most one sweep per sandbox, and
// the sandbox's next export cancels it and waits for it before it starts.
// That only keeps the two from sharing the store's request quota; lineage
// generations make a sweep safe beside any later export, on any node. A
// sweep lost in a restart is logged and left to the next one.

// States of xfss3SweepRecord.
const (
	xfss3SweepPending   = "pending"
	xfss3SweepDone      = "done"
	xfss3SweepCancelled = "cancelled"
	xfss3SweepFailed    = "failed"
	xfss3SweepLost      = "lost"
)

// xfss3SweepRecord is the sweep after a checkpoint, in its job record.
type xfss3SweepRecord struct {
	State      string                 `json:"state"`
	StartedAt  string                 `json:"started_at,omitempty"`
	FinishedAt string                 `json:"finished_at,omitempty"`
	Error      string                 `json:"error,omitempty"`
	Stats      *checkpoint.SweepStats `json:"stats,omitempty"`
}

type xfss3Sweep struct {
	sandboxID  string
	snapshotID string
	cancel     context.CancelFunc
	// gate opens once the job is recorded exported; the sweep's own record
	// is written after that one.
	gate chan struct{}
	once sync.Once
	done chan struct{}
}

var xfss3Sweeps sync.Map // sandbox id -> *xfss3Sweep

// Test hooks.
var (
	xfss3SweepLineage  = checkpoint.SweepLineage
	xfss3SweepStopWait = 30 * time.Second
)

// newXFSS3Sweep registers the sweep after checkpoint m of snapshotID, whose
// job lives in home under session. It runs once release is called.
func newXFSS3Sweep(ctx context.Context, client *checkpoint.Client, home, session, snapshotID string, m *checkpoint.Manifest) *xfss3Sweep {
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &xfss3Sweep{sandboxID: m.SandboxID, snapshotID: snapshotID, cancel: cancel,
		gate: make(chan struct{}), done: make(chan struct{})}
	if prev, loaded := xfss3Sweeps.Swap(m.SandboxID, s); loaded {
		prev.(*xfss3Sweep).cancel()
	}
	go s.run(runCtx, client, home, session, m)
	return s
}

// release lets the sweep start.
func (s *xfss3Sweep) release() { s.once.Do(func() { close(s.gate) }) }

func (s *xfss3Sweep) run(ctx context.Context, client *checkpoint.Client, home, session string, m *checkpoint.Manifest) {
	defer close(s.done)
	defer s.cancel()
	defer xfss3Sweeps.CompareAndDelete(s.sandboxID, s)
	logger := log.G(ctx).WithField("snapshot", s.snapshotID)
	<-s.gate
	rec := &xfss3SweepRecord{StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if ctx.Err() != nil {
		rec.State = xfss3SweepCancelled
		logger.Infof("xfs-s3 sweep of lineage %s after %s: cancelled before it started", s.sandboxID, s.snapshotID)
	} else {
		cfg := client.Config()
		st, err := xfss3SweepLineage(ctx, client, m, checkpoint.SweepOptions{
			Workers: xfss3Workers("CUBE_XFSS3_SWEEP_WORKERS", 4), RequestsPerSecond: cfg.SweepRate()})
		rec.Stats = st
		switch {
		case st != nil && st.Cancelled:
			rec.State = xfss3SweepCancelled
		case err != nil:
			rec.State = xfss3SweepFailed
		default:
			rec.State = xfss3SweepDone
		}
		if err != nil {
			rec.Error = err.Error()
			logger.Warnf("xfs-s3 sweep of lineage %s after %s: %v", s.sandboxID, s.snapshotID, err)
		}
		if st == nil {
			st = &checkpoint.SweepStats{}
		}
		logger.Infof("xfs-s3 sweep of lineage %s after %s %s: deleted %d of %d superseded chunks (listed %d, failed %d) in %s, %d requests at most %d/s, batched=%t, throttled 429=%d 503=%d backoff=%s",
			s.sandboxID, s.snapshotID, rec.State, st.Deleted, st.Candidates, st.Listed, st.Failed,
			time.Duration(st.DurationMS)*time.Millisecond, st.Requests, cfg.SweepRate(), st.Batched,
			st.Throttled429, st.Throttled503, time.Duration(st.ThrottleBackoffMS)*time.Millisecond)
	}
	rec.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := updateXFSS3Job(home, session, func(j *xfss3Job) bool {
		j.Sweep = rec
		return true
	}); err != nil {
		// The package may be gone already: a resumed checkpoint is retired.
		logger.Debugf("xfs-s3 sweep of %s not recorded: %v", s.snapshotID, err)
	}
}

// stopXFSS3Sweep cancels the sweep of sandboxID running on this node, if
// any, and waits for it, before the export of snapshot next.
func stopXFSS3Sweep(ctx context.Context, sandboxID, next string) {
	v, ok := xfss3Sweeps.Load(sandboxID)
	if !ok {
		return
	}
	s := v.(*xfss3Sweep)
	start := time.Now()
	s.cancel()
	select {
	case <-s.done:
		log.G(ctx).Infof("xfs-s3 export of %s stopped the sweep of lineage %s after %s in %s",
			next, sandboxID, s.snapshotID, time.Since(start).Round(time.Millisecond))
	case <-time.After(xfss3SweepStopWait):
		log.G(ctx).Warnf("xfs-s3 export of %s: the sweep of lineage %s after %s did not stop within %s; exporting beside it",
			next, sandboxID, s.snapshotID, xfss3SweepStopWait)
	case <-ctx.Done():
	}
}

var xfss3JobLocks sync.Map // package home -> *sync.Mutex

// updateXFSS3Job applies change to the job record in home, if it still
// belongs to session (any session when empty), and writes it back when
// change says so. Writers that may race with a running export or sweep go
// through here; the export itself owns the record while it runs.
func updateXFSS3Job(home, session string, change func(*xfss3Job) bool) error {
	v, _ := xfss3JobLocks.LoadOrStore(home, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	job, err := readXFSS3Job(home)
	if err != nil {
		return err
	}
	if session != "" && job.Session != session {
		return fmt.Errorf("job of %s now belongs to session %s", home, job.Session)
	}
	if !change(job) {
		return nil
	}
	return writeXFSS3Job(home, job)
}

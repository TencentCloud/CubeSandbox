// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// SweepOptions bounds one lineage sweep. A sweep runs in the background
// after its checkpoint is committed, beside exports and imports that share
// the store's request quota, so it is paced rather than fast.
type SweepOptions struct {
	// Workers is how many delete requests run at once; zero means 4.
	Workers int
	// RequestsPerSecond caps every request of the sweep, its listing and
	// retries included; zero means DefaultSweepRequestsPerSecond.
	RequestsPerSecond int
	// BatchSize is the most keys one DeleteObjects request carries; zero
	// means MaxBatchDelete.
	BatchSize int
}

// SweepStats is what one sweep did.
type SweepStats struct {
	Listed     int `json:"listed"`
	Candidates int `json:"candidates"`
	Deleted    int `json:"deleted"`
	Failed     int `json:"failed"`
	// Requests counts every request sent, retries included.
	Requests int64 `json:"requests"`
	// Batched is whether the deletes went out as DeleteObjects.
	Batched           bool  `json:"batched"`
	Cancelled         bool  `json:"cancelled"`
	DurationMS        int64 `json:"duration_ms"`
	Throttled429      int64 `json:"throttled_429"`
	Throttled503      int64 `json:"throttled_503"`
	ThrottleBackoffMS int64 `json:"throttle_backoff_ms"`
}

// SweepLineage deletes what checkpoint keep superseded in its sandbox's
// lineage: every chunk of keep's generation or an older one that keep does
// not name, chunks from before generations included. It deletes only keys
// it listed and never one of a newer generation, so it is safe beside any
// export built from keep or a later checkpoint, on this node or another,
// and a sweep cancelled midway or lost in a restart leaves only garbage
// that the next sweep deletes.
//
// Keys go out in DeleteObjects batches when the store has it, else one by
// one, opt.Workers requests at a time and opt.RequestsPerSecond at most. A
// cancelled sweep returns its stats with Cancelled set and no error; keys
// it could not delete are counted in Failed and reported in the error.
func SweepLineage(ctx context.Context, c *Client, keep *Manifest, opt SweepOptions) (*SweepStats, error) {
	start := time.Now()
	st := &SweepStats{}
	if keep == nil || keep.SchemaVersion != SchemaVersionIncremental || keep.Generation <= 0 {
		return st, fmt.Errorf("only a checkpoint with a lineage generation can be swept after")
	}
	if err := ValidateID("sandbox id", keep.SandboxID); err != nil {
		return st, err
	}
	opt = opt.withDefaults()
	ctx, hooks := withRequestHooks(ctx, float64(opt.RequestsPerSecond))
	defer func() {
		st.Requests = hooks.requests.Load()
		th := hooks.throttles()
		st.Throttled429, st.Throttled503, st.ThrottleBackoffMS = th.Status429, th.Status503, th.Backoff.Milliseconds()
		st.Cancelled = ctx.Err() != nil
		st.DurationMS = time.Since(start).Milliseconds()
	}()
	referenced := map[string]bool{}
	for _, f := range keep.Files {
		for _, ch := range f.Chunks {
			referenced[ch.Key] = true
		}
	}
	lineage := LineagePrefix(c.Prefix(), keep.SandboxID)
	objs, err := c.ListObjects(ctx, lineage)
	if err != nil {
		if ctx.Err() != nil {
			return st, nil
		}
		return st, fmt.Errorf("list %s: %w", lineage, err)
	}
	st.Listed = len(objs)
	var doomed []string
	for _, o := range objs {
		gen, _, ok := parseLineageKey(lineage, o.Key)
		if !ok || gen > keep.Generation || referenced[o.Key] {
			continue
		}
		doomed = append(doomed, o.Key)
	}
	st.Candidates = len(doomed)

	var firstErr error
	st.Deleted, st.Failed, st.Batched, firstErr = deleteKeys(ctx, c, doomed, opt)
	if st.Failed > 0 {
		return st, fmt.Errorf("%d of %d superseded chunks were not deleted: %w", st.Failed, st.Candidates, firstErr)
	}
	return st, nil
}

// withDefaults fills the zero fields of opt.
func (opt SweepOptions) withDefaults() SweepOptions {
	if opt.Workers <= 0 {
		opt.Workers = 4
	}
	if opt.RequestsPerSecond <= 0 {
		opt.RequestsPerSecond = DefaultSweepRequestsPerSecond
	}
	if opt.BatchSize <= 0 || opt.BatchSize > MaxBatchDelete {
		opt.BatchSize = MaxBatchDelete
	}
	return opt
}

// deleteKeys deletes keys in DeleteObjects batches of opt.BatchSize when the
// store has it, else one by one, opt.Workers requests at a time. It returns
// how many keys it deleted and failed to delete, whether the deletes went
// out as batches, and the first failure. Keys it never got to because ctx
// ended count as neither.
func deleteKeys(ctx context.Context, c *Client, keys []string, opt SweepOptions) (deleted, failed int, batched bool, firstErr error) {
	var (
		mu     sync.Mutex
		single []string
	)
	fail := func(n int, err error) {
		mu.Lock()
		defer mu.Unlock()
		failed += n
		if firstErr == nil {
			firstErr = err
		}
	}
	done := func(n int) {
		mu.Lock()
		defer mu.Unlock()
		deleted += n
	}
	if c.BatchDeleteSupported() {
		var batches [][]string
		for i := 0; i < len(keys); i += opt.BatchSize {
			batches = append(batches, keys[i:min(i+opt.BatchSize, len(keys))])
		}
		forEachIndex(ctx, opt.Workers, len(batches), func(i int) {
			b := batches[i]
			refused, err := c.DeleteObjects(ctx, b)
			switch {
			case errors.Is(err, ErrBatchDeleteUnsupported):
				mu.Lock()
				single = append(single, b...)
				mu.Unlock()
			case err != nil:
				if ctx.Err() == nil {
					fail(len(b), err)
				}
			default:
				n := 0
				for _, k := range b {
					if e, ok := refused[k]; ok {
						fail(1, e)
						n++
					}
				}
				done(len(b) - n)
			}
		})
		batched = c.BatchDeleteSupported()
	} else {
		single = keys
	}
	forEachIndex(ctx, opt.Workers, len(single), func(i int) {
		if err := c.Delete(ctx, single[i]); err != nil {
			if ctx.Err() == nil {
				fail(1, err)
			}
			return
		}
		done(1)
	})
	return deleted, failed, batched, firstErr
}

// forEachIndex runs fn(0..n-1) on up to workers goroutines and stops handing
// out work once ctx is done.
func forEachIndex(ctx context.Context, workers, n int, fn func(i int)) {
	if n == 0 {
		return
	}
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(workers, n); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				fn(i)
			}
		}()
	}
feed:
	for i := 0; i < n; i++ {
		select {
		case next <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
}

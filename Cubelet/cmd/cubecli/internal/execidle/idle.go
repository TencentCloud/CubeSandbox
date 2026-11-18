// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package execidle watches stdin/stdout activity for cubecli interactive exec so
// abandoned -it sessions do not keep forbidding Pause.
package execidle

import (
	"context"
	"io"
	"sync/atomic"
	"time"
)

// DefaultTTYTimeout is the default I/O idle limit for cubecli exec -t.
const DefaultTTYTimeout = 15 * time.Minute

// Activity records the last time stdin or stdout delivered bytes.
type Activity struct {
	lastActive atomic.Int64
}

// NewActivity starts with "now" as the last activity time.
func NewActivity() *Activity {
	a := &Activity{}
	a.Touch()
	return a
}

// Touch records activity at the current wall clock.
func (a *Activity) Touch() {
	a.lastActive.Store(time.Now().UnixNano())
}

// LastActive is when stdin or stdout last delivered bytes.
func (a *Activity) LastActive() time.Time {
	return time.Unix(0, a.lastActive.Load())
}

// ActivityReader wraps stdin and records reads on the shared Activity.
type ActivityReader struct {
	r io.Reader
	a *Activity
}

// NewActivityReader returns a reader that touches a on each successful read.
func NewActivityReader(r io.Reader, a *Activity) *ActivityReader {
	return &ActivityReader{r: r, a: a}
}

func (ar *ActivityReader) Read(p []byte) (int, error) {
	n, err := ar.r.Read(p)
	if n > 0 {
		ar.a.Touch()
	}
	return n, err
}

// ActivityWriter wraps stdout and records writes on the shared Activity.
type ActivityWriter struct {
	w io.Writer
	a *Activity
}

// NewActivityWriter returns a writer that touches a on each successful write.
func NewActivityWriter(w io.Writer, a *Activity) *ActivityWriter {
	return &ActivityWriter{w: w, a: a}
}

func (aw *ActivityWriter) Write(p []byte) (int, error) {
	n, err := aw.w.Write(p)
	if n > 0 {
		aw.a.Touch()
	}
	return n, err
}

// Watch starts a watchdog that invokes onIdle when lastActive stays past
// timeout. onIdle returns true to stop the watchdog (e.g. Kill succeeded) or
// false to retry on a later tick while still idle — so a transient Kill failure
// does not permanently disarm the watcher. It returns a channel closed when the
// watchdog exits (including the disabled / nil-arg path). Callers must wait on
// that channel after cancelling ctx before trusting any side effects from
// onIdle. timeout <= 0 disables.
func Watch(ctx context.Context, timeout time.Duration, lastActive func() time.Time, onIdle func() bool) <-chan struct{} {
	done := make(chan struct{})
	if timeout <= 0 || lastActive == nil || onIdle == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if time.Since(lastActive()) >= timeout {
					if onIdle() {
						return
					}
				}
			}
		}
	}()
	return done
}

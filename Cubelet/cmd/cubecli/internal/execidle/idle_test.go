// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package execidle

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivityReaderTouchesOnRead(t *testing.T) {
	r, w := io.Pipe()
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	activity := NewActivity()
	ar := NewActivityReader(r, activity)
	before := activity.LastActive()
	time.Sleep(2 * time.Millisecond)

	go func() {
		_, _ = w.Write([]byte("x"))
	}()
	buf := make([]byte, 1)
	n, err := ar.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.True(t, activity.LastActive().After(before))
}

func TestActivityWriterTouchesOnWrite(t *testing.T) {
	activity := NewActivity()
	before := activity.LastActive()
	time.Sleep(2 * time.Millisecond)

	var buf []byte
	aw := NewActivityWriter(writerFunc(func(p []byte) (int, error) {
		buf = append(buf, p...)
		return len(p), nil
	}), activity)
	n, err := aw.Write([]byte("out"))
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, []byte("out"), buf)
	assert.True(t, activity.LastActive().After(before))
}

func TestWatchFiresWhenIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	fired := make(chan struct{}, 1)
	start := time.Now().Add(-2 * time.Second)
	done := Watch(ctx, 50*time.Millisecond, func() time.Time { return start }, func() bool {
		fired <- struct{}{}
		return true
	})

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("idle watchdog did not fire")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle watchdog did not exit after firing")
	}
}

func TestWatchRespectsCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fired := make(chan struct{}, 1)
	done := Watch(ctx, time.Hour, time.Now, func() bool {
		fired <- struct{}{}
		return true
	})
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("idle watchdog did not exit after cancel")
	}
	select {
	case <-fired:
		t.Fatal("idle watchdog must not fire after cancel")
	default:
	}
}

func TestWatchDisabledWhenZero(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fired := make(chan struct{}, 1)
	done := Watch(ctx, 0, func() time.Time { return time.Time{} }, func() bool {
		fired <- struct{}{}
		return true
	})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled watchdog must close done immediately")
	}
	select {
	case <-fired:
		t.Fatal("idle watchdog must stay disabled when timeout is 0")
	default:
	}
}

func TestWatchRetriesUntilOnIdleDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var attempts atomic.Int32
	start := time.Now().Add(-2 * time.Second)
	done := Watch(ctx, 50*time.Millisecond, func() time.Time { return start }, func() bool {
		return attempts.Add(1) >= 2
	})

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("idle watchdog did not exit after onIdle returned true")
	}
	assert.GreaterOrEqual(t, attempts.Load(), int32(2))
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

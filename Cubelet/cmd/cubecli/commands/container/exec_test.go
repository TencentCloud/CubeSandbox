// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package container

import (
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStdinCloserClosesOnceOnEOF(t *testing.T) {
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = reader.Close()
	})
	require.NoError(t, writer.Close())

	stdin := newStdinCloser(reader)
	var closeCount atomic.Int32
	stdin.SetCloser(func() {
		closeCount.Add(1)
	})

	n, err := stdin.Read(make([]byte, 8))
	assert.Equal(t, 0, n)
	assert.ErrorIs(t, err, io.EOF)
	assert.Equal(t, int32(1), closeCount.Load())

	n, err = stdin.Read(make([]byte, 8))
	assert.Equal(t, 0, n)
	assert.ErrorIs(t, err, io.EOF)
	assert.Equal(t, int32(1), closeCount.Load())
}

func TestWaitForProcessIOWaitsAfterExit(t *testing.T) {
	statusC := make(chan containerd.ExitStatus, 1)
	statusC <- *containerd.NewExitStatus(1, time.Now(), nil)
	outputDrained := false

	status := waitForProcessIO(statusC, func() { outputDrained = true })

	assert.True(t, outputDrained)
	code, _, err := status.Result()
	require.NoError(t, err)
	assert.Equal(t, uint32(1), code)
}

// TestWaitForProcessIOReturnsOnlyAfterDrain guards the ordering invariant that
// caused the original race: the exit status must not be returned to the caller
// (which triggers process.Delete) until the IO drain has completed. A drain that
// is still running when waitForProcessIO returns would let deletion truncate the
// final stdout/stderr copy loops.
func TestWaitForProcessIOReturnsOnlyAfterDrain(t *testing.T) {
	statusC := make(chan containerd.ExitStatus, 1)
	statusC <- *containerd.NewExitStatus(0, time.Now(), nil)

	drainStarted := make(chan struct{})
	releaseDrain := make(chan struct{})
	var drainReturned atomic.Bool

	returned := make(chan struct{})
	go func() {
		waitForProcessIO(statusC, func() {
			close(drainStarted)
			<-releaseDrain
			drainReturned.Store(true)
		})
		close(returned)
	}()

	<-drainStarted
	// The drain is in flight; waitForProcessIO must still be blocked.
	select {
	case <-returned:
		t.Fatal("waitForProcessIO returned before IO drain completed")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseDrain)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("waitForProcessIO did not return after IO drain completed")
	}
	assert.True(t, drainReturned.Load(), "drain must have finished before return")
}

// TestWaitForProcessIODrainsAfterStatus asserts the drain observes the exit
// status first (status is consumed from the channel before wait runs), matching
// the "<-statusC then wait()" order in the implementation. The status is loaded
// synchronously before the call so that a reordered "wait(); <-statusC" would
// still see it buffered and fail the assertion.
func TestWaitForProcessIODrainsAfterStatus(t *testing.T) {
	statusC := make(chan containerd.ExitStatus, 1)
	statusC <- *containerd.NewExitStatus(3, time.Now(), nil)
	var statusReadyAtDrain atomic.Bool

	status := waitForProcessIO(statusC, func() {
		// The status must already be drained from the channel before wait runs.
		statusReadyAtDrain.Store(len(statusC) == 0)
	})

	assert.True(t, statusReadyAtDrain.Load())
	code, _, err := status.Result()
	require.NoError(t, err)
	assert.Equal(t, uint32(3), code)
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestLeaderSupervisorFastFailuresExitAtThreshold(t *testing.T) {
	s := newLeaderSupervisor(3, 15*time.Second)
	boom := errors.New("boom")

	assert.False(t, s.Record(boom, time.Second), "first fast failure should step down, not exit")
	assert.False(t, s.Record(boom, time.Second), "second fast failure should step down, not exit")
	assert.True(t, s.Record(boom, time.Second), "third consecutive fast failure should exit")
	assert.Equal(t, 3, s.Fails())
}

func TestLeaderSupervisorCleanStintResets(t *testing.T) {
	s := newLeaderSupervisor(3, 15*time.Second)
	boom := errors.New("boom")

	assert.False(t, s.Record(boom, time.Second))
	assert.False(t, s.Record(boom, time.Second))

	// A clean finish (leadership lost normally, or shutdown) resets the count.
	assert.False(t, s.Record(nil, time.Minute))
	assert.False(t, s.Record(context.Canceled, time.Minute))
	assert.Equal(t, 0, s.Fails())

	// So the next failure run starts from scratch.
	assert.False(t, s.Record(boom, time.Second))
	assert.False(t, s.Record(boom, time.Second))
	assert.True(t, s.Record(boom, time.Second))
}

func TestLeaderSupervisorStableStintFailureResets(t *testing.T) {
	s := newLeaderSupervisor(3, 15*time.Second)
	boom := errors.New("boom")

	assert.False(t, s.Record(boom, time.Second))
	assert.False(t, s.Record(boom, time.Second))

	// A stint that survived past stableAfter before failing counts as
	// healthy: transient mid-run failures must not march the process
	// towards exit.
	assert.False(t, s.Record(boom, 16*time.Second))
	assert.Equal(t, 0, s.Fails())

	assert.False(t, s.Record(boom, time.Second))
	assert.False(t, s.Record(boom, time.Second))
	assert.True(t, s.Record(boom, time.Second))
}

func TestLeaderSupervisorContextWrappedCancelIsClean(t *testing.T) {
	s := newLeaderSupervisor(3, 15*time.Second)
	wrapped := errors.Join(errors.New("loop done"), context.Canceled)
	assert.False(t, s.Record(wrapped, time.Second))
	assert.Equal(t, 0, s.Fails())
}

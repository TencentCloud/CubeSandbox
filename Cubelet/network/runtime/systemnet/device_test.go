// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package systemnet

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestWithDumpRetrySucceedsAfterInterrupt(t *testing.T) {
	calls := 0
	got, err := WithDumpRetry(func() (int, error) {
		calls++
		if calls < 3 {
			return 0, netlink.ErrDumpInterrupted
		}
		return 42, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 42, got)
	assert.Equal(t, 3, calls)
}

func TestWithDumpRetryExhaustsAttempts(t *testing.T) {
	calls := 0
	_, err := WithDumpRetry(func() (int, error) {
		calls++
		return 0, netlink.ErrDumpInterrupted
	})
	require.ErrorIs(t, err, netlink.ErrDumpInterrupted)
	assert.Equal(t, maxDumpRetries, calls)
}

func TestWithDumpRetryDoesNotRetryOtherErrors(t *testing.T) {
	want := errors.New("not a dump interrupt")
	calls := 0
	_, err := WithDumpRetry(func() (int, error) {
		calls++
		return 0, want
	})
	require.ErrorIs(t, err, want)
	assert.Equal(t, 1, calls)
}

func TestWithDumpRetryTreatsEINTRAsInterrupt(t *testing.T) {
	calls := 0
	got, err := WithDumpRetry(func() (string, error) {
		calls++
		if calls == 1 {
			return "", unix.EINTR
		}
		return "ok", nil
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", got)
	assert.Equal(t, 2, calls)
}

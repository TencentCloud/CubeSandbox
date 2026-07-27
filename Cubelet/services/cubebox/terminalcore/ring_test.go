// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package terminalcore

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRingBufferOffsetsAndTruncation(t *testing.T) {
	ring := newRingBuffer(8)
	require.Equal(t, uint64(0), ring.Write([]byte("abcdef")))
	require.Equal(t, uint64(6), ring.Write([]byte("ghijkl")))
	require.Equal(t, uint64(4), ring.Start())
	require.Equal(t, uint64(12), ring.End())

	data, from, truncated := ring.ReadFrom(0)
	require.True(t, truncated)
	require.Equal(t, uint64(4), from)
	require.Equal(t, []byte("efghijkl"), data)

	data, from, truncated = ring.ReadFrom(9)
	require.False(t, truncated)
	require.Equal(t, uint64(9), from)
	require.Equal(t, []byte("jkl"), data)

	data, from, truncated = ring.ReadFrom(99)
	require.False(t, truncated)
	require.Equal(t, uint64(12), from)
	require.Empty(t, data)
}

func TestRingBufferWriteLargerThanCapacity(t *testing.T) {
	ring := newRingBuffer(4)
	require.Equal(t, uint64(0), ring.Write([]byte("abcdefgh")))
	require.Equal(t, uint64(4), ring.Start())
	require.Equal(t, uint64(8), ring.End())
	data, from, truncated := ring.ReadFrom(0)
	require.True(t, truncated)
	require.Equal(t, uint64(4), from)
	require.Equal(t, []byte("efgh"), data)
}

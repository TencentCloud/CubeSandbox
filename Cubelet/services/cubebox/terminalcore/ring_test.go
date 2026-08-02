// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package terminalcore

import (
	"bytes"
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

func TestRingBufferPreservesOrderAcrossMultipleWraps(t *testing.T) {
	ring := newRingBuffer(8)
	for _, chunk := range []string{"abc", "def", "ghi", "jkl", "mno"} {
		ring.Write([]byte(chunk))
	}

	data, from, truncated := ring.ReadFrom(0)
	require.True(t, truncated)
	require.Equal(t, uint64(7), from)
	require.Equal(t, []byte("hijklmno"), data)

	data, from, truncated = ring.ReadFrom(10)
	require.False(t, truncated)
	require.Equal(t, uint64(10), from)
	require.Equal(t, []byte("klmno"), data)
}

func BenchmarkRingBufferSteadyStateWrite(b *testing.B) {
	ring := newRingBuffer(256 << 10)
	chunk := bytes.Repeat([]byte{'x'}, 32<<10)
	ring.Write(bytes.Repeat([]byte{'w'}, 256<<10))
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ring.Write(chunk)
	}
}

func BenchmarkRingBufferReplay(b *testing.B) {
	ring := newRingBuffer(256 << 10)
	ring.Write(bytes.Repeat([]byte{'x'}, 256<<10))
	b.SetBytes(256 << 10)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data, _, _ := ring.ReadFrom(ring.Start())
		if len(data) != 256<<10 {
			b.Fatalf("unexpected replay length: %d", len(data))
		}
	}
}

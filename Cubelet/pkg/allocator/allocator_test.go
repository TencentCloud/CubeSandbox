// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package allocator

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllocator(t *testing.T) {
	ranger, err := NewSimpleLinearRanger(1, 2)
	require.NoError(t, err)

	alloc := NewAllocator[uint16](ranger)

	assert.False(t, alloc.Has(1))
	assert.False(t, alloc.Has(2))

	v, err := alloc.Allocate(nil)
	assert.NoError(t, err)
	assert.Equal(t, uint16(1), v)
	assert.True(t, alloc.Has(1))

	err = alloc.Assign(1)
	assert.ErrorIs(t, err, ErrAllocated)

	err = alloc.Assign(100)
	assert.ErrorIs(t, err, ErrOutOfRange)

	v, err = alloc.Allocate(nil)
	assert.NoError(t, err)
	assert.Equal(t, uint16(2), v)
	assert.True(t, alloc.Has(2))

	_, err = alloc.Allocate(nil)
	assert.ErrorIs(t, err, ErrExhausted)

	alloc.Release(1)

	v, err = alloc.Allocate(nil)
	assert.NoError(t, err)
	assert.Equal(t, uint16(1), v)
	assert.True(t, alloc.Has(1))
}

func BenchmarkAllocator(b *testing.B) {
	ranger, err := NewSimpleLinearRanger(2000, 65535)
	require.NoError(b, err)

	alloc := NewAllocator[uint16](ranger)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			v, err := alloc.Allocate(nil)
			if err == nil {
				alloc.Release(v)
			}
		}
	})

	fmt.Println(len(alloc.store))
}

func TestAllocatorExpand(t *testing.T) {
	ranger, err := NewSimpleLinearRanger(1, 2)
	require.NoError(t, err)

	alloc := NewAllocator[uint16](ranger)

	v, err := alloc.Allocate(nil)
	assert.NoError(t, err)
	assert.Equal(t, uint16(1), v)
	assert.True(t, alloc.Has(1))

	v, err = alloc.Allocate(nil)
	assert.NoError(t, err)
	assert.Equal(t, uint16(2), v)
	assert.True(t, alloc.Has(2))

	var expand uint16
	v, err = alloc.Allocate(func() error {
		expand, _ = ranger.Expand()
		return nil
	})
	assert.NoError(t, err)
	assert.Equal(t, expand, v)
	assert.True(t, alloc.Has(expand))

	_, err = alloc.Allocate(nil)
	assert.ErrorIs(t, err, ErrExhausted)
}

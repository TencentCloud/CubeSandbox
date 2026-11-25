// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package allocator

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name  string
		lower uint16
		upper uint16
		err   error
	}{
		{
			name:  "valid range",
			lower: 100,
			upper: 200,
			err:   nil,
		},
		{
			name:  "invalid range",
			lower: 200,
			upper: 100,
			err:   ErrInvalidRange,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSimpleLinearRanger(tt.lower, tt.upper)
			assert.Equal(t, tt.err, err)
		})
	}
}

func TestRanger_Contains(t *testing.T) {
	r := &SimpleLinearRanger{lower: 100, upper: 200}

	tests := []struct {
		name     string
		rangeID  uint16
		expected bool
	}{
		{
			name:     "within range",
			rangeID:  150,
			expected: true,
		},
		{
			name:     "lower bound",
			rangeID:  100,
			expected: true,
		},
		{
			name:     "upper bound",
			rangeID:  200,
			expected: true,
		},
		{
			name:     "below range",
			rangeID:  50,
			expected: false,
		},
		{
			name:     "above range",
			rangeID:  250,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual := r.Contains(tt.rangeID)
			assert.Equal(t, tt.expected, actual)
		})
	}
}

func TestIterator(t *testing.T) {
	r := &SimpleLinearRanger{lower: 100, upper: 102, cur: -1}
	iter := r.GetIter()

	tests := []struct {
		name     string
		expected uint16
	}{
		{
			name:     "first",
			expected: 100,
		},
		{
			name:     "second",
			expected: 101,
		},
		{
			name:     "third",
			expected: 102,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, iter.Get())
			iter.Next()
		})
	}

	assert.Nil(t, iter.Next())
}

func TestEmptyRangerGrowthAndRecovery(t *testing.T) {
	r := NewEmptySimpleLinearRanger()
	a := NewAllocator[uint16](r)
	assert.Equal(t, 0, r.Cap())
	assert.False(t, r.Contains(0))
	assert.ErrorIs(t, a.Assign(0), ErrOutOfRange)
	for want := uint16(0); want < 3; want++ {
		id, err := a.Allocate(func() error { _, err := r.Expand(); return err })
		assert.NoError(t, err)
		assert.Equal(t, want, id)
	}
	a.Release(1)
	id, err := a.Allocate(nil)
	assert.NoError(t, err)
	assert.Equal(t, uint16(1), id)

	recovered := NewEmptySimpleLinearRanger()
	recovered.ExpandTo(0)
	assert.Equal(t, 1, recovered.Cap())
	assert.True(t, recovered.Contains(0))
	recovered.ExpandTo(65535)
	assert.Equal(t, 65536, recovered.Cap())
	_, err = recovered.Expand()
	assert.ErrorIs(t, err, ErrExhausted)
}

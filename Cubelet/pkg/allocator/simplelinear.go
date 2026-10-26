// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package allocator

import (
	"errors"
	"math"
)

var (
	ErrInvalidRange = errors.New("upper is bigger than lower")
)

type SimpleLinearRanger struct {
	lower uint16
	upper uint16

	cur   int
	start int
}

func NewSimpleLinearRanger(lower, upper uint16) (*SimpleLinearRanger, error) {
	if lower > upper {
		return nil, ErrInvalidRange
	}

	return &SimpleLinearRanger{
		lower: lower,
		upper: upper,
		cur:   -1,
	}, nil
}

func (r *SimpleLinearRanger) Contains(rangeID uint16) bool {
	return (rangeID >= r.lower) && (rangeID <= r.upper)
}

func (r *SimpleLinearRanger) Cap() int {
	return int(r.upper - r.lower + 1)
}

func (r *SimpleLinearRanger) Expand() (uint16, error) {
	if r.upper == math.MaxUint16 {
		return 0, ErrExhausted
	}
	r.upper += 1
	return r.upper, nil
}

func (r *SimpleLinearRanger) ExpandTo(upper uint16) {
	if r.upper < upper {
		r.upper = upper
	}
}

func (r *SimpleLinearRanger) GetIter() RangeIterator[uint16] {
	r.start = r.cur

	if r.cur == -1 {
		r.cur = int(r.lower)
		return r
	}

	r.moveCurForward()
	return r
}

func (r *SimpleLinearRanger) moveCurForward() {
	if uint16(r.cur) == r.upper {
		r.cur = int(r.lower)
	} else {
		r.cur++
	}
}

func (r *SimpleLinearRanger) Get() uint16 {
	return uint16(r.cur)
}

func (r *SimpleLinearRanger) Next() *uint16 {
	if r.cur == r.start {
		return nil
	}

	if r.start == -1 && r.cur == int(r.upper) {
		return nil
	}

	r.moveCurForward()
	v := uint16(r.cur)
	return &v
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package terminalcore

// ringBuffer retains the newest bytes while assigning every byte a monotonic
// absolute offset. It is only accessed while a session mutex is held.
type ringBuffer struct {
	buf   []byte
	start uint64
	end   uint64
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{buf: make([]byte, 0, capacity)}
}

func (r *ringBuffer) Write(p []byte) uint64 {
	offset := r.end
	r.end += uint64(len(p))
	capacity := cap(r.buf)
	if capacity == 0 || len(p) == 0 {
		return offset
	}

	if len(p) >= capacity {
		r.buf = append(r.buf[:0], p[len(p)-capacity:]...)
		r.start = r.end - uint64(capacity)
		return offset
	}

	overflow := len(r.buf) + len(p) - capacity
	if overflow > 0 {
		copy(r.buf, r.buf[overflow:])
		r.buf = r.buf[:len(r.buf)-overflow]
		r.start += uint64(overflow)
	}
	r.buf = append(r.buf, p...)
	return offset
}

func (r *ringBuffer) ReadFrom(offset uint64) (data []byte, from uint64, truncated bool) {
	from = offset
	if from < r.start {
		from = r.start
		truncated = true
	}
	if from > r.end {
		from = r.end
	}
	if from == r.end {
		return nil, from, truncated
	}
	index := int(from - r.start)
	return append([]byte(nil), r.buf[index:]...), from, truncated
}

func (r *ringBuffer) Start() uint64 { return r.start }
func (r *ringBuffer) End() uint64   { return r.end }

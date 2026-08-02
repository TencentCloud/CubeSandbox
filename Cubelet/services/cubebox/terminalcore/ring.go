// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package terminalcore

// ringBuffer retains the newest bytes while assigning every byte a monotonic
// absolute offset. It is only accessed while a session mutex is held.
type ringBuffer struct {
	buf   []byte
	head  int
	size  int
	start uint64
	end   uint64
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{buf: make([]byte, capacity)}
}

func (r *ringBuffer) Write(p []byte) uint64 {
	offset := r.end
	r.end += uint64(len(p))
	capacity := len(r.buf)
	if capacity == 0 || len(p) == 0 {
		return offset
	}

	if len(p) >= capacity {
		copy(r.buf, p[len(p)-capacity:])
		r.head = 0
		r.size = capacity
		r.start = r.end - uint64(capacity)
		return offset
	}

	overflow := r.size + len(p) - capacity
	if overflow > 0 {
		r.head = (r.head + overflow) % capacity
		r.size -= overflow
		r.start += uint64(overflow)
	}

	tail := (r.head + r.size) % capacity
	first := min(len(p), capacity-tail)
	copy(r.buf[tail:], p[:first])
	copy(r.buf, p[first:])
	r.size += len(p)
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
	length := int(r.end - from)
	data = make([]byte, length)
	index := (r.head + int(from-r.start)) % len(r.buf)
	first := min(length, len(r.buf)-index)
	copy(data, r.buf[index:index+first])
	copy(data[first:], r.buf[:length-first])
	return data, from, truncated
}

func (r *ringBuffer) Start() uint64 { return r.start }
func (r *ringBuffer) End() uint64   { return r.end }

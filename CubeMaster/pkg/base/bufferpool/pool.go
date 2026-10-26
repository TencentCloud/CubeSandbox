// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package bufferpool provides a pool of bytes.Buffer
package bufferpool

import (
	"bytes"
	"sync"
)

type BufferPool interface {
	Put(buffer *bytes.Buffer)
	Get() *bytes.Buffer
}

type defaultPool struct {
	pool *sync.Pool
}

func (p *defaultPool) Put(buf *bytes.Buffer) {
	if buf != nil {
		buf.Reset()
		p.pool.Put(buf)
	}
}

func (p *defaultPool) Get() *bytes.Buffer {
	b := p.pool.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

func New(size int) BufferPool {
	if size == 0 {
		size = 8192
	}
	bufferPool := &defaultPool{
		pool: &sync.Pool{
			New: func() interface{} {
				return bytes.NewBuffer(make([]byte, size))
			},
		},
	}
	return bufferPool
}

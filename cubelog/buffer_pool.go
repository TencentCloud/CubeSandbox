// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package CubeLog

import (
	"bytes"
	"sync"
)

var (
	bufferPool BufferPool
)

type BufferPool interface {
	Put(buffer *bytes.Buffer)
	Get() *bytes.Buffer
}

type defaultPool struct {
	pool *sync.Pool
}

func (p *defaultPool) Put(buf *bytes.Buffer) {
	p.pool.Put(buf)
}

func (p *defaultPool) Get() *bytes.Buffer {
	return p.pool.Get().(*bytes.Buffer)
}

func getBuffer() *bytes.Buffer {
	b := bufferPool.Get()
	b.Reset()
	return b
}

func putBuffer(buf *bytes.Buffer) {
	buf.Reset()
	bufferPool.Put(buf)
}

func init() {
	bufferPool = &defaultPool{
		pool: &sync.Pool{
			New: func() interface{} {
				return new(bytes.Buffer)
			},
		},
	}
}

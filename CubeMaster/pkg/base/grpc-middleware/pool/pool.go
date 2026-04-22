// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package pool TODO
package pool

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var ErrClosed = errors.New("pool is closed")

type Pool interface {
	Get() (Conn, error)

	GetActiveTimeAndRef() (time.Time, int32)

	Close() error

	GracefulStop(maxWaitTime time.Duration)

	Status() string
}

type pool struct {
	index uint32

	current int32

	ref int32

	opt Options

	conns []*conn

	address string
	ua      string

	active time.Time

	sync.RWMutex
}

func (p *pool) GetActiveTimeAndRef() (time.Time, int32) {
	return p.active, p.ref
}

func New(ua, address string, option Options) (Pool, error) {
	if address == "" {
		return nil, errors.New("invalid address settings")
	}
	if option.Dial == nil {
		return nil, errors.New("invalid dial settings")
	}
	if option.MaxIdle <= 0 || option.MaxActive <= 0 || option.MaxIdle > option.MaxActive {
		return nil, errors.New("invalid maximum settings")
	}

	if option.OptionType == SingleConn {
		p := &pool{
			index:   0,
			current: int32(1),
			ref:     0,
			opt:     option,
			conns:   make([]*conn, 1),
			address: address,
			ua:      ua,
		}
		c, err := p.opt.Dial(ua, address)
		if err != nil {
			_ = p.Close()
			return nil, err
		}
		p.conns[0] = p.wrapConn(c, false)
		return p, nil
	}

	if option.MaxConcurrentStreams <= 0 {
		return nil, errors.New("invalid maximum settings")
	}

	p := &pool{
		index:   0,
		current: int32(option.MaxIdle),
		ref:     0,
		opt:     option,
		conns:   make([]*conn, option.MaxActive),
		address: address,
		ua:      ua,
	}

	for i := 0; i < p.opt.MaxIdle; i++ {
		c, err := p.opt.Dial(ua, address)
		if err != nil {
			_ = p.Close()
			return nil, fmt.Errorf("dial [%s] is not able to fill the pool: %s", address, err)
		}
		p.conns[i] = p.wrapConn(c, false)
	}

	return p, nil
}

func (p *pool) incrRef() int32 {
	newRef := atomic.AddInt32(&p.ref, 1)
	if newRef >= math.MaxInt32 {
		newRef = math.MaxInt32
	}
	return newRef
}

func (p *pool) decrRef() {
	if p == nil {
		return
	}

	newRef := atomic.AddInt32(&p.ref, -1)
	if newRef < 0 {
		newRef = 0
	}

	if newRef == 0 && atomic.LoadInt32(&p.current) > int32(p.opt.MaxIdle) {
		p.Lock()
		if atomic.LoadInt32(&p.ref) == 0 {
			atomic.StoreInt32(&p.current, int32(p.opt.MaxIdle))
			p.deleteFrom(p.opt.MaxIdle)
		}
		p.Unlock()
	}

}

func (p *pool) reset(index int) {
	conn := p.conns[index]
	if conn == nil {
		return
	}
	_ = conn.reset()
	p.conns[index] = nil
}

func (p *pool) deleteFrom(begin int) {
	maxActive := p.opt.MaxActive
	if p.opt.OptionType == SingleConn {
		maxActive = 1
	}

	for i := begin; i < maxActive; i++ {
		p.reset(i)
	}
}

func (p *pool) Get() (Conn, error) {
	defer func() {
		p.Lock()
		p.active = time.Now()
		p.Unlock()
	}()

	nextRef := p.incrRef()
	current := atomic.LoadInt32(&p.current)
	if current == 0 {
		return nil, ErrClosed
	}

	if p.opt.OptionType == SingleConn {
		return p.CheckConnStatus(p.conns[0])
	}

	if nextRef <= int32(p.opt.MaxIdle)*int32(p.opt.MaxConcurrentStreams) {
		next := atomic.AddUint32(&p.index, 1) % uint32(p.opt.MaxIdle)
		return p.CheckConnStatus(p.conns[next])
	}

	if nextRef <= current*int32(p.opt.MaxConcurrentStreams) {
		next := atomic.AddUint32(&p.index, 1) % uint32(current)
		return p.CheckConnStatus(p.conns[next])
	}

	if current >= int32(p.opt.MaxActive) {

		if p.opt.Reuse {
			next := atomic.AddUint32(&p.index, 1) % uint32(current)
			return p.CheckConnStatus(p.conns[next])
		}

		c, err := p.opt.Dial(p.ua, p.address)
		return p.wrapConn(c, true), err
	}

	p.Lock()
	current = atomic.LoadInt32(&p.current)
	if current < int32(p.opt.MaxActive) && nextRef > current*int32(p.opt.MaxConcurrentStreams) {

		increment := current
		if current+increment > int32(p.opt.MaxActive) {
			increment = int32(p.opt.MaxActive) - current
		}
		var i int32
		var err error
		for i = 0; i < increment; i++ {
			c, er := p.opt.Dial(p.ua, p.address)
			if er != nil {
				err = er
				break
			}
			p.reset(int(current + i))
			p.conns[current+i] = p.wrapConn(c, false)
		}
		current += i
		atomic.StoreInt32(&p.current, current)
		if err != nil {
			p.Unlock()
			return nil, err
		}
	}
	p.Unlock()
	next := atomic.AddUint32(&p.index, 1) % uint32(current)
	return p.CheckConnStatus(p.conns[next])
}

func (p *pool) Close() error {
	atomic.StoreUint32(&p.index, 0)
	atomic.StoreInt32(&p.current, 0)
	atomic.StoreInt32(&p.ref, 0)
	p.deleteFrom(0)
	return nil
}

func (p *pool) GracefulStop(maxWaitTime time.Duration) {
	done := make(chan struct{}, 1)

	go func() {
		for {
			if atomic.LoadInt32(&p.ref) <= 0 {
				p.Close()
				done <- struct{}{}
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	select {
	case <-done:
		return
	case <-time.After(maxWaitTime):
		p.Close()
		return
	}
}

func (p *pool) CheckConnStatus(conn *conn) (*conn, error) {

	return conn, nil
}

func (p *pool) Status() string {
	return fmt.Sprintf("address:%s, index:%d, current:%d, ref:%d. option:%v",
		p.address, p.index, p.current, p.ref, p.opt)
}

func (p *pool) IsNetErr(err error) bool {
	if err != nil && status.Code(err) == codes.Unavailable {
		return true
	}

	return false
}

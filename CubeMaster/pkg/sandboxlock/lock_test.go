// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandboxlock

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeRedis struct {
	mu    sync.Mutex
	store map[string]string
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{store: map[string]string{}}
}

func (f *fakeRedis) Do(cmd string, args ...interface{}) (interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch cmd {
	case "SET":
		key := args[0].(string)
		val := args[1].(string)
		nx := false
		for _, a := range args[2:] {
			if s, ok := a.(string); ok && s == "NX" {
				nx = true
			}
		}
		if nx {
			if _, ok := f.store[key]; ok {
				return nil, nil
			}
		}
		f.store[key] = val
		return "OK", nil
	case "DEL":
		key := args[0].(string)
		delete(f.store, key)
		return int64(1), nil
	default:
		return nil, errors.New("unexpected cmd " + cmd)
	}
}

func TestWithLockSerializes(t *testing.T) {
	r := newFakeRedis()
	ctx := context.Background()
	var order []int
	var mu sync.Mutex
	started := make(chan struct{})
	releaseFirst := make(chan struct{})

	errCh := make(chan error, 2)
	go func() {
		errCh <- withLock(ctx, r, "k", "a", Options{TTL: 10 * time.Second, RetryInterval: 10 * time.Millisecond}, func(context.Context) error {
			close(started)
			<-releaseFirst
			mu.Lock()
			order = append(order, 1)
			mu.Unlock()
			return nil
		})
	}()
	<-started
	go func() {
		errCh <- withLock(ctx, r, "k", "b", Options{TTL: 10 * time.Second, RetryInterval: 10 * time.Millisecond}, func(context.Context) error {
			mu.Lock()
			order = append(order, 2)
			mu.Unlock()
			return nil
		})
	}()
	time.Sleep(30 * time.Millisecond)
	close(releaseFirst)
	if err := <-errCh; err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("second lock: %v", err)
	}
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("order=%v want [1 2]", order)
	}
}

func TestWithLockContextCancel(t *testing.T) {
	r := newFakeRedis()
	r.store["k"] = "held"
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err := withLock(ctx, r, "k", "waiter", Options{TTL: 10 * time.Second, RetryInterval: 10 * time.Millisecond}, func(context.Context) error {
		t.Fatal("fn should not run")
		return nil
	})
	if !errors.Is(err, ErrLockNotAcquired) {
		t.Fatalf("err=%v want ErrLockNotAcquired", err)
	}
}

func TestDefaultTTLIs10s(t *testing.T) {
	if DefaultTTL != 10*time.Second {
		t.Fatalf("DefaultTTL=%v want 10s", DefaultTTL)
	}
}

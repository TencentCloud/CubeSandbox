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
	case "EVAL":
		// EVAL script numkeys key [key...] arg [arg...]
		if len(args) < 4 {
			return nil, errors.New("EVAL needs script, numkeys, key, token")
		}
		key := args[2].(string)
		token := args[3].(string)
		if f.store[key] == token {
			delete(f.store, key)
			return int64(1), nil
		}
		return int64(0), nil
	case "DEL":
		return nil, errors.New("sandboxlock must unlock via EVAL, not DEL")
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

func TestUnlockDoesNotDeleteForeignToken(t *testing.T) {
	r := newFakeRedis()
	r.store["k"] = "holder-b"
	if err := unlock(r, "k", "holder-a"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if got := r.store["k"]; got != "holder-b" {
		t.Fatalf("store=%q want holder-b (stale unlock must not steal lock)", got)
	}
}

func TestUnlockDeletesOwnToken(t *testing.T) {
	r := newFakeRedis()
	r.store["k"] = "holder-a"
	if err := unlock(r, "k", "holder-a"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if _, ok := r.store["k"]; ok {
		t.Fatal("expected key deleted")
	}
}

func TestDefaultTTLIs60s(t *testing.T) {
	if DefaultTTL != 60*time.Second {
		t.Fatalf("DefaultTTL=%v want 60s", DefaultTTL)
	}
}

func TestLockTokenIncludesDebugPrefix(t *testing.T) {
	tok := lockToken("pause")
	if len(tok) < len("pause:")+10 || tok[:6] != "pause:" {
		t.Fatalf("token=%q want pause:<uuid>", tok)
	}
}

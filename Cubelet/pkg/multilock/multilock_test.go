// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package multilock

import (
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMultiLock_Get(t *testing.T) {
	o := NewMultiLockOptions()
	mlock := NewMultiLock(o)

	key := "test_key"
	mlock.Get(key)
	value, ok := mlock.Load(key)
	if !ok {
		t.Errorf("Lock at key '%s' not found", key)
	}
	l, ok := value.(*rwLock)
	if !ok {
		t.Errorf("Returned value is not an rwLock")
	}
	if l.key != key {
		t.Errorf("Lock key '%s' does not match expected key '%s'", l.key, key)
	}

	l.LockAt()
	l.UnlockAt()
}

func TestMultiLock_Lifetime(t *testing.T) {
	o := NewMultiLockOptions()
	o.CheckInterval = 100 * time.Millisecond
	o.ExpiredInSecond = 3
	mlock := NewMultiLock(o)
	key := "test_key"
	l := mlock.Get(key)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.LockAt()
			defer l.UnlockAt()
			_ = uuid.New().String()
		}()
	}
	wg.Wait()

	lock := l.(*rwLock)
	atomic.StoreInt64(&lock.updateAt, time.Now().Unix()-o.ExpiredInSecond-1)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := mlock.Load(key); !ok {
			l.LockAt()
			l.UnlockAt()
			return
		}
		time.Sleep(o.CheckInterval)
	}
	t.Fatalf("Lock at key '%s' was not removed after expiration", key)
}

func TestMultiLock_LockAt(t *testing.T) {
	o := NewMultiLockOptions()
	mlock := NewMultiLock(o)

	key := "test_key"
	l := mlock.Get(key)

	freadTimeout := time.After(2 * time.Second)
	fread := func() {
		l.LockAt()
		defer l.UnlockAt()

		time.Sleep(2 * time.Second)
	}

	got := make(chan bool)
	fw := func() {
		l.Lock()
		defer l.Unlock()
		<-freadTimeout
		close(got)
	}
	go fread()
	go fw()
	select {
	case <-got:
		t.Logf("successfully locked")
		return
	case <-time.After(3 * time.Second):
		t.Error("Failed to lock read and write simultaneously.")
	}
}

func Benchmark_RLock(b *testing.B) {
	o := NewMultiLockOptions()
	o.CheckInterval = 10 * time.Millisecond
	mlock := NewMultiLock(o)

	fn := func() {
		key := "test_key"
		l := mlock.Get(key)
		l.LockAt()
		defer l.UnlockAt()

	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fn()
	}
	b.StopTimer()
}

func Benchmark_Parallel_SameKey_RLock(b *testing.B) {
	o := NewMultiLockOptions()
	o.CheckInterval = 10 * time.Millisecond
	mlock := NewMultiLock(o)
	fn := func() {
		key := "test_key"
		l := mlock.Get(key)
		l.LockAt()
		defer l.UnlockAt()

	}
	b.SetParallelism(runtime.GOMAXPROCS(-1))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			fn()
		}
	})
	b.StopTimer()
}

func Benchmark_Parallel_MultiKey_RLock(b *testing.B) {
	o := NewMultiLockOptions()
	o.CheckInterval = 10 * time.Millisecond
	mlock := NewMultiLock(o)
	fn := func(key string) {
		l := mlock.Get(key)
		l.LockAt()
		defer l.UnlockAt()

	}
	var keys []string
	keyLen := 10000
	for i := 0; i < keyLen; i++ {
		keys = append(keys, fmt.Sprintf("%d", rand.Int63n(int64(keyLen))))
	}
	b.SetParallelism(runtime.GOMAXPROCS(-1))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		k := keys[rand.Int()%keyLen]
		for pb.Next() {
			fn(k)
		}
	})
	b.StopTimer()
}

func Benchmark_Parallel_SameKey_WLock(b *testing.B) {
	o := NewMultiLockOptions()
	o.CheckInterval = 10 * time.Millisecond
	mlock := NewMultiLock(o)
	fn := func() {
		key := "test_key"
		l := mlock.Get(key)
		l.LockAt()
		_ = 10
		l.UnlockAt()

		l.Lock()
		_ = uuid.New().String()
		l.Unlock()

	}
	b.SetParallelism(50)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			fn()
		}
	})
	b.StopTimer()
}

func Benchmark_Parallel_MultiKey_WLock(b *testing.B) {
	o := NewMultiLockOptions()
	o.CheckInterval = 10 * time.Millisecond
	mlock := NewMultiLock(o)
	fn := func(key string) {
		l := mlock.Get(key)
		l.Lock()
		defer l.Unlock()

	}
	var keys []string
	keyLen := 10000
	for i := 0; i < keyLen; i++ {
		keys = append(keys, fmt.Sprintf("%d", rand.Int63n(int64(keyLen))))
	}
	b.SetParallelism(50)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		k := keys[rand.Int()%keyLen]
		for pb.Next() {
			fn(k)
		}
	})
	b.StopTimer()
}

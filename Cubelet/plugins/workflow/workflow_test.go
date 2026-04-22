// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package workflow

import (
	"context"
	"fmt"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

func TestLimiter(t *testing.T) {
	runcc := int64(4)
	limiter := semaphore.NewWeighted(runcc)
	assert.NotNil(t, limiter)
	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	wg.Add(int(runcc))
	job := func(i int) error {
		if err := limiter.Acquire(context.Background(), 1); err != nil {
			wg.Done()
			return err
		}
		defer limiter.Release(1)
		wg.Done()
		log.Printf("running %d", i)
		defer log.Printf("end %d", i)
		for {
			select {
			case <-ctx.Done():
				return nil
			}
		}
	}

	for i := 0; i < int(runcc); i++ {
		go func(j int) {
			err := job(j)
			if err != nil {
				assert.FailNow(t, "should not be err")
			}
		}(i)
	}
	wg.Wait()

	ctxd, dcancel := context.WithTimeout(context.Background(), time.Second)
	defer dcancel()
	if err := limiter.Acquire(ctxd, 1); err == nil {
		assert.FailNow(t, "should be err")
	}
	assert.False(t, limiter.TryAcquire(1))
	cancel()
	time.Sleep(time.Second)
}

func TestErrgroupWithCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var got error
	unexpected := make(chan struct{})
	go func() {
		eg, ctxWithCancel := errgroup.WithContext(ctx)
		for j := 0; j < 10; j++ {
			i := j
			eg.Go(func() error {
				select {
				case <-ctxWithCancel.Done():
					return ctxWithCancel.Err()
				case <-time.After(5 * time.Second):
					return fmt.Errorf("timeout %d", i)
				}
			})
		}
		got = eg.Wait()
		if got != nil && got != context.Canceled {
			unexpected <- struct{}{}
		}
	}()
	time.AfterFunc(3*time.Second, func() {

		cancel()
	})
	time.Sleep(6 * time.Second)
	select {
	case <-ctx.Done():
		if ctx.Err() != context.Canceled {
			t.Fatalf("ctx.Err() != context.Canceled")
		}
	case <-unexpected:
		t.Fatal("unexpected")
	}
}

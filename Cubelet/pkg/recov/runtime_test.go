// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
Package recov support recover handler
Copyright (c) 2020 Tencent Serverless
* All rights reserved
* Author: jiangdu
* Date: 2020-06-08
*/
package recov

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestHandleWithCrash(t *testing.T) {
	isPanic := false
	_ = func(ctx context.Context) error {
		defer HandleCrash(func(panicError interface{}) {
			fmt.Println(DumpStacktrace(3, panicError))
			isPanic = true
		})
		fmt.Println("Test_HandleCrash")
		panic(any(errors.New("MockError")))
	}(nil)
	if !isPanic {
		t.Errorf("should be %t, actual %t", true, isPanic)
	}
}

func TestHandleWithoutCrash(t *testing.T) {
	isPanic := false
	func() {
		defer HandleCrash(func(panicError interface{}) {
			isPanic = true
		})
		fmt.Println("Test_HandleCrash")
	}()
	if isPanic {
		t.Errorf("should be %t, actual %t", false, isPanic)
	}
}

func Test_GlobalHandler(t *testing.T) {
	hasGlobalPanic := false
	RegisterGlobalHandler(func(i interface{}) {
		hasGlobalPanic = true
	})
	func() {
		defer HandleCrash()
		panic(any(errors.New("MockError")))
	}()
	if !hasGlobalPanic {
		t.Error("RegisterGlobalHandler is not as expected")
	}
}

func TestGoWithRecoverWithCrash(t *testing.T) {
	group := sync.WaitGroup{}
	group.Add(1)
	hasPanicHandle := false
	hasRun := false
	GoWithRecover(func() {
		fmt.Println("no panic")
		hasRun = true
		panic(any(errors.New("MockError")))
	}, func(panicError interface{}) {
		hasPanicHandle = true
		group.Done()
	})
	group.Wait()
	if !hasRun {
		t.Errorf("should be %t, actual %t", true, hasRun)
	}

	if !hasPanicHandle {
		t.Errorf("should be %t, actual %t", true, hasPanicHandle)
	}
}

func TestGoWithWgWithCrash(t *testing.T) {
	wg := &sync.WaitGroup{}
	errCnt := 0
	GoWithWaitGroup(wg, func() {
		panic(any("MockPanic"))
	}, func(panicError interface{}) {
		errCnt++
	})
	wg.Wait()
	assert.Equal(t, 1, errCnt)
}

func TestGoWithRecoverWithoutCrash(t *testing.T) {
	group := sync.WaitGroup{}
	group.Add(1)
	hasPanicHandle := false
	hasFunRun := false
	GoWithRecover(func() {
		fmt.Println("no panic")
		hasFunRun = true
		group.Done()
	}, func(panicError interface{}) {
		hasPanicHandle = true
	})
	group.Wait()
	if !hasFunRun {
		t.Errorf("should be %t, actual %t", true, hasFunRun)
	}
	if hasPanicHandle {
		t.Errorf("should be %t, actual %t", false, hasPanicHandle)
	}
}

func TestGoWithRetryWithoutCrash(t *testing.T) {
	var panicTime int32 = 0
	GoWithRetry(func() {
		fmt.Println("no panic")
	}, 3, func(panicError interface{}) {
		atomic.AddInt32(&panicTime, 1)
	})
	time.Sleep(2 * time.Second)
	if panicTime != 0 {
		t.Errorf("should be %d, actual %d", 0, 1)
	}
}

func TestGoWithRetryWithCrash(t *testing.T) {
	retryTime := 3
	var panicTime int32 = 0
	GoWithRetry(func() {
		fmt.Println("panic")
		panic(any(errors.New("RetryPanic")))
	}, retryTime, func(panicError interface{}) {
		atomic.AddInt32(&panicTime, 1)
	})
	time.Sleep(2 * time.Second)
	if int(panicTime) != retryTime {
		t.Errorf("should be %d, actual %d", 0, 1)
	}
}

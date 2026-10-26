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
	"sync"
)

var globalHandlers = []func(interface{}){}

func RegisterGlobalHandler(handler func(interface{})) {
	globalHandlers = append(globalHandlers, handler)
}

func HandleCrash(additionalHandlers ...func(panicError interface{})) {
	if r := recover(); r != any(nil) {
		for _, fn := range globalHandlers {
			fn(r)
		}
		for _, fn := range additionalHandlers {
			fn(r)
		}
	}
}

func GoWithRecover(handler func(), panicHandlers ...func(panicError interface{})) {
	go WithRecover(handler, panicHandlers...)
}

func WithRecover(handler func(), panicHandlers ...func(panicError interface{})) {
	func() {
		defer HandleCrash(panicHandlers...)
		handler()
	}()
}

func GoWithRetry(handler func(), retries int, panicHandlers ...func(panicError interface{})) {
	go WithRetry(handler, retries, panicHandlers...)
}

func WithRetry(handler func(), retries int, panicHandlers ...func(panicError interface{})) {
	func() {
		tryNum := 0
		hasPanic := false
		handlers := append(panicHandlers, func(panicError interface{}) {
			tryNum++
			hasPanic = true
		})
		for tryNum < retries {
			func() {
				defer HandleCrash(handlers...)
				handler()
			}()
			if !hasPanic {
				return
			}
			hasPanic = false
		}
	}()
}

func GoWithWaitGroup(wg *sync.WaitGroup, handler func(), panicHandlers ...func(panicError interface{})) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		WithRecover(handler, panicHandlers...)
	}()
}

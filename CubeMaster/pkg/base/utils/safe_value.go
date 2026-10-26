// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package utils

import (
	"reflect"
	"sync"
)

func SafeValue[T any](v T) T {
	vt := reflect.TypeOf(v)
	switch vt.Kind() {
	case reflect.Ptr:
		vv := reflect.ValueOf(v)
		if vv.IsNil() {
			return reflect.New(vt.Elem()).Interface().(T)
		}
		return v
	default:
		return v
	}
}

type AtomicMapStat struct {
	sync.Mutex
	m map[string]int
}

func (a *AtomicMapStat) Add(id string, n int) {
	a.Lock()
	defer a.Unlock()
	if a.m == nil {
		a.m = make(map[string]int)
	}
	if _, ok := a.m[id]; !ok {
		a.m[id] = n
	} else {
		a.m[id] += n
	}
}

func (a *AtomicMapStat) Get(id string) int {
	a.Lock()
	defer a.Unlock()
	if a.m == nil {
		return 0
	}
	if _, ok := a.m[id]; !ok {
		return 0
	}
	return a.m[id]
}

func (a *AtomicMapStat) Has(id string) bool {
	a.Lock()
	defer a.Unlock()
	if a.m == nil {
		a.m = make(map[string]int)
		return false
	}
	_, ok := a.m[id]
	return ok
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package util TODO
package util

import (
	"bytes"
	"encoding/gob"
	"time"
)

type CacheValue struct {
	Key        string
	Value      interface{}
	LastAccess int64
	Expired    time.Duration
}

func (value *CacheValue) Size() int64 {
	size := (len(value.Key) + 16) * 2

	size += 16
	size += 32
	return int64(size)
}

func Sizeof(v interface{}) int {
	b := new(bytes.Buffer)
	if err := gob.NewEncoder(b).Encode(v); err != nil {
		return 0
	}
	return b.Len()

}

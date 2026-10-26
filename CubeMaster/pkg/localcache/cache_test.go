// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package localcache

import (
	"math/rand"
	"testing"

	"github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/assert"
)

func TestRandomGet(t *testing.T) {
	testC := cache.New(0, 0)
	allIds := map[string]int{}
	testnum := 20
	for i := 0; i < testnum; i++ {
		id := randSeq(12)
		testC.SetDefault(id, i)
		allIds[id] = 0
	}

	testtimes := 1000
	for i := 0; i < testtimes; i++ {
		elems := testC.Items()
		cnt := 0
		for k := range elems {
			if cnt == 10 {
				break
			}
			allIds[k]++
			cnt++
		}
	}
	got := 0
	expected := testtimes * 10
	for _, v := range allIds {
		got += v
	}
	assert.Equal(t, expected, got)
	t.Logf("allIds: %v", allIds)
}

var letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

func randSeq(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

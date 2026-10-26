// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package retry

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/wait"
)

func TestRetry(t *testing.T) {
	var defaultRetry = &wait.Backoff{
		Steps:    30,
		Duration: 800 * time.Millisecond,
		Factor:   1.1,
		Jitter:   0.5,
	}
	cnt := int32(0)
	start := time.Now()
	init := time.Now()
	err := wait.ExponentialBackoff(*defaultRetry, func() (bool, error) {
		t.Logf("retry:%d,sleep:%v,cost:%v", atomic.AddInt32(&cnt, 1), time.Since(start), time.Since(init))
		start = time.Now()
		return false, nil
	})
	t.Logf("init:%v,cost:%v", init, time.Since(init))
	assert.True(t, wait.Interrupted(err))
}

func TestBackoff(t *testing.T) {
	cnt := int32(0)
	start := time.Now()
	init := time.Now()
	err := DoWithBackoff(func() error {
		t.Logf("retry:%d,sleep:%v", atomic.AddInt32(&cnt, 1), time.Since(start))
		start = time.Now()
		return fmt.Errorf("retry")
	}, nil)
	t.Logf("init:%v,cost:%v", init, time.Since(init))
	assert.NotNil(t, err)
}

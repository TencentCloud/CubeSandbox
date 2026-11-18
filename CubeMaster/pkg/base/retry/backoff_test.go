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
)

func TestBackoff(t *testing.T) {
	backoff := defaultBackoff()
	backoff.Duration = time.Millisecond
	cnt := int32(0)
	start := time.Now()
	init := time.Now()
	err := DoWithBackoff(func() error {
		t.Logf("retry:%d,sleep:%v", atomic.AddInt32(&cnt, 1), time.Since(start))
		start = time.Now()
		return fmt.Errorf("retry")
	}, &backoff)
	t.Logf("init:%v,cost:%v", init, time.Since(init))
	assert.NotNil(t, err)
	assert.Equal(t, int32(backoff.Steps), cnt)
}

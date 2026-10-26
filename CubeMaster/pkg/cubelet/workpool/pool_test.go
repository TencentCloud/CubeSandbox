// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/**
*
* Copyright (c) 2020 Tencent Serverless
* All rights reserved
* Author: jiangdu
* Date: 2020-10-29
 */
package workpool

import (
	"testing"
	"time"
)

func TestExecute(t *testing.T) {
	poolSize := 5
	pool := NewWorkerPool(poolSize)
	p := pool.(*workerPool)
	for i := 0; i < 100; i++ {
		pool.Exec(func() {
			time.Sleep(time.Millisecond)
		})
	}
	time.Sleep(time.Millisecond * 20)
	if len(p.semCh) != 0 {
		t.Errorf("want poolSize: 0, actual %d", len(p.semCh))
	}

}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package retry

import (
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

func DoWithBackoff(do func() error, bf *wait.Backoff) error {
	var defaultRetry = wait.Backoff{
		Steps:    20,
		Duration: 500 * time.Millisecond,
		Factor:   1.0,
		Jitter:   0.5,
	}
	if bf != nil {
		defaultRetry = *bf
	}
	var lastErr error
	for defaultRetry.Steps > 0 {
		if er := do(); er != nil {
			lastErr = er
		} else {
			lastErr = nil
			break
		}
		if defaultRetry.Steps == 1 {
			break
		}
		time.Sleep(defaultRetry.Step())
	}
	return lastErr
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"net/http"
	"time"
)

func isRetryableStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		status >= http.StatusInternalServerError
}

func backoffDuration(delivery DeliveryConfig, failedAttempt int) time.Duration {
	exponent := failedAttempt - 1
	if exponent < 0 {
		exponent = 0
	}
	if exponent > 16 {
		exponent = 16
	}
	millis := uint64(delivery.InitialBackoffMS) << exponent
	maxMillis := uint64(delivery.MaxBackoffSecs) * 1_000
	if millis > maxMillis {
		millis = maxMillis
	}
	return time.Duration(millis) * time.Millisecond
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package checkpoint

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// bandwidthBurst bounds how far transfers may run ahead of the limit.
const bandwidthBurst = 4 << 20

var bandwidth struct {
	sync.Mutex
	limit *rate.Limiter
}

// SetBandwidthLimit caps the bytes per second that every Client of this
// process moves, uploads and downloads together: one limit per node, shared
// by exports, readbacks and restores. A store that throttles per account
// (answering 429 above a bandwidth quota) then sees each node stay below
// its share instead of every transfer retrying. Zero or less removes it.
func SetBandwidthLimit(bytesPerSecond int64) {
	bandwidth.Lock()
	defer bandwidth.Unlock()
	if bytesPerSecond <= 0 {
		bandwidth.limit = nil
		return
	}
	bandwidth.limit = rate.NewLimiter(rate.Limit(bytesPerSecond), bandwidthBurst)
}

// BandwidthLimit returns the current limit in bytes per second, 0 if none.
func BandwidthLimit() int64 {
	bandwidth.Lock()
	defer bandwidth.Unlock()
	if bandwidth.limit == nil {
		return 0
	}
	return int64(bandwidth.limit.Limit())
}

// bandwidthWaited sums the time every transfer of this process spent held
// back by the limit; a caller diffs it around a transfer to see how much of
// that transfer the limit cost.
var bandwidthWaited atomic.Int64

// BandwidthWaited is the total time transfers have waited on the limit.
func BandwidthWaited() time.Duration { return time.Duration(bandwidthWaited.Load()) }

func waitBandwidth(ctx context.Context, n int) error {
	bandwidth.Lock()
	l := bandwidth.limit
	bandwidth.Unlock()
	if l == nil {
		return nil
	}
	start := time.Now()
	defer func() { bandwidthWaited.Add(int64(time.Since(start))) }()
	for n > 0 {
		k := min(n, bandwidthBurst)
		if err := l.WaitN(ctx, k); err != nil {
			return err
		}
		n -= k
	}
	return nil
}

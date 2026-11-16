// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package jfsdiff

import (
	"context"
	"time"
)

// WarmStats describes one WarmFile run.
type WarmStats struct {
	SizeBytes      int64
	ResidentBefore int64 // bytes already in the page cache when called
	ReadBytes      int64 // bytes read to fill the page cache
	Skipped        bool  // resident fraction was already above the threshold
	Duration       time.Duration
}

const (
	warmChunk          = 8 << 20
	defaultWarmWorkers = 8
)

// WarmFile pulls path into the kernel page cache before a VM maps it.
//
// Guest memory on the jfs backend is a MAP_PRIVATE mapping of a file on the
// JuiceFS FUSE mount. A cold fault costs a FUSE round trip per page, so a
// cross-node restore of a large memory image can take seconds to tens of
// seconds and trip the guest agent's reconnect deadline. Warming only
// JuiceFS's local block cache does not help: the FUSE round trip remains.
// Reading the file sequentially in parallel fills the kernel page cache that
// the hypervisor's later page faults are served from.
//
// Chunks already resident are skipped (mincore), and nothing is read when at
// least skipFraction of the file is resident, so a same-node restore costs
// almost nothing.
func WarmFile(ctx context.Context, path string, workers int, skipFraction float64) (WarmStats, error) {
	if workers <= 0 {
		workers = defaultWarmWorkers
	}
	return warmFile(ctx, path, workers, skipFraction)
}

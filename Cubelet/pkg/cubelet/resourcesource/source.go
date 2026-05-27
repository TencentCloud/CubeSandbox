// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package resourcesource provides a late-bound registry for the cubelet's
// node-resource reporting pipeline.
//
// The cubebox service plugin owns the canonical sandbox ledger and is the
// only place that can answer how much CPU, memory and disk has been
// allocated on this node. The cubelet plugin builds heartbeat payloads
// destined for cubemaster but lives in a layer that must not import the
// cubebox service (this would create an init cycle and tangle the plugin
// graph). This package brokers between the two halves without forcing a
// strict initialisation order: the cubebox service calls Set once it is
// ready, and the cubelet calls Get on every heartbeat tick.
//
// When Get returns nil the cubelet skips the resource fields gracefully and
// continues emitting conditions / images / templates, so partial bring-up
// (e.g. during cold start, or in unit tests with no cubebox plugin) cannot
// take heartbeats offline.
package resourcesource

import "sync/atomic"

// AllocatedResources mirrors masterclient.AllocatedResources. Keeping a
// pure-data twin here avoids importing masterclient from cubebox, which
// would re-create the layering issue this package exists to solve.
type AllocatedResources struct {
	MilliCPU      int64
	MemoryMB      int64
	MvmNum        int64
	MvmRunningNum int64
	NicQueues     int64
	DataDiskMB    int64
	StorageDiskMB int64
}

// DiskUsage mirrors masterclient.DiskUsage with the same rationale as above.
type DiskUsage struct {
	DataDiskUsagePer    float64
	StorageDiskUsagePer float64
	SysDiskUsagePer     float64
}

// Collector reports the resource view of a cubelet node. Implementations
// must be safe for concurrent invocation from the heartbeat goroutine.
type Collector interface {
	// CollectAllocated returns the aggregated sandbox-quota view. Returning
	// nil signals "no data this tick" and the caller will skip the field.
	CollectAllocated() *AllocatedResources

	// CollectDiskUsage returns observed filesystem fill ratios. Returning
	// nil signals "no data" and the caller will skip the field.
	CollectDiskUsage() *DiskUsage
}

var current atomic.Value

// Set wires a collector for the lifetime of the process. Calling Set with
// nil clears the registration (useful for tests).
func Set(c Collector) {
	if c == nil {
		current.Store((*holder)(nil))
		return
	}
	current.Store(&holder{c: c})
}

// Get returns the current collector or nil when none has been registered.
func Get() Collector {
	v := current.Load()
	if v == nil {
		return nil
	}
	h, _ := v.(*holder)
	if h == nil {
		return nil
	}
	return h.c
}

type holder struct{ c Collector }

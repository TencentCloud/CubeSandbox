// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package filter

import (
	"context"
	"sync/atomic"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
)

// rejectSampleRate is 1/N: the count of rejects that stayed unprinted is
// summarised on only one request in N. Without sampling the summary is itself
// a per-request line, which is what made the original per-budget design
// expensive under a clone burst.
const rejectSampleRate = 64

// rejectSampler spreads summary logging across requests.
var rejectSampler atomic.Uint64

// rejectLog counts the per-node rejections one filter invocation produced.
// The number of lines actually printed is bounded per request (not per filter)
// by the shared budget on the selector context, so four concurrently running
// filters cannot print four separate sets of lines for the same candidate set.
type rejectLog struct {
	ctx    context.Context
	selCtx *selctx.SelectorCtx
	hidden int
}

func newRejectLog(selCtx *selctx.SelectorCtx) *rejectLog {
	var ctx context.Context
	if selCtx != nil {
		ctx = selCtx.Ctx
	}
	return &rejectLog{ctx: ctx, selCtx: selCtx}
}

func (r *rejectLog) warnf(format string, args ...any) {
	if !r.acquire() {
		return
	}
	log.G(r.ctx).Warnf(format, args...)
}

func (r *rejectLog) errorf(format string, args ...any) {
	if !r.acquire() {
		return
	}
	log.G(r.ctx).Errorf(format, args...)
}

// acquire reserves a shared per-request budget slot. Rejections beyond the
// budget are counted and reported by flush instead of printed one per node.
func (r *rejectLog) acquire() bool {
	if r == nil {
		return false
	}
	if r.selCtx != nil && r.selCtx.TakeRejectLogSlot() {
		return true
	}
	r.hidden++
	return false
}

// flush reports how many rejects stayed unprinted, on a sampled subset of
// requests so the summary cannot itself become a per-request line.
func (r *rejectLog) flush(id string) {
	if r == nil || r.hidden == 0 {
		return
	}
	if rejectSampler.Add(1)%rejectSampleRate != 0 {
		return
	}
	log.G(r.ctx).Warnf("%s suppressed %d similar node rejects", id, r.hidden)
}

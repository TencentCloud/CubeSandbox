// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package filter

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

// rejectLogLimit is how many per-node reject lines one filter invocation
// prints before collapsing the rest into a single summary.
const rejectLogLimit = 3

type rejectLog struct {
	ctx    context.Context
	shown  int
	hidden int
}

func newRejectLog(ctx context.Context) *rejectLog {
	return &rejectLog{ctx: ctx}
}

func (r *rejectLog) warnf(format string, args ...any) {
	r.emit(log.G(r.ctx).Warnf, format, args...)
}

func (r *rejectLog) errorf(format string, args ...any) {
	r.emit(log.G(r.ctx).Errorf, format, args...)
}

func (r *rejectLog) emit(write func(string, ...any), format string, args ...any) {
	if r.shown < rejectLogLimit {
		write(format, args...)
		r.shown++
		return
	}
	r.hidden++
}

func (r *rejectLog) flush(id string) {
	if r == nil || r.hidden == 0 {
		return
	}
	log.G(r.ctx).Warnf("%s suppressed %d similar node rejects", id, r.hidden)
}

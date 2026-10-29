// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"strings"
	"sync/atomic"
)

// Destruction owns pre-cleanup fields. Identity/time are assigned only at Complete.
type Destruction struct {
	sandbox, template string
	fields            LifecycleFields
	origin            Origin
	published         atomic.Bool
}

func (h *PublisherHolder) PrepareDestruction(ctx context.Context, sandbox, template, reason string, f LifecycleFields) *Destruction {
	if !h.Enabled() || !OriginFromContext(ctx).Allows(TypeKilled) {
		return nil
	}
	f.KillReason = reason
	if len(sandbox) > maxStringBytes || len(template) > maxStringBytes || validateFields(f) != nil {
		captureCounters[1].Inc()
		return nil
	}
	return &Destruction{sandbox: strings.Clone(sandbox), template: strings.Clone(template), fields: cloneFields(f), origin: OriginFromContext(ctx)}
}
func (d *Destruction) Complete(h *PublisherHolder) {
	if d == nil || !d.published.CompareAndSwap(false, true) {
		return
	}
	h.PublishContext(WithOrigin(context.Background(), d.origin), TypeKilled, d.sandbox, d.template, d.fields)
}

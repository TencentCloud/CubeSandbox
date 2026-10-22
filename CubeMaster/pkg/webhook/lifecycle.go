// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"math/big"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	maxMetadataEntries = 64
	maxMetadataBytes   = 16 * 1024
)

// LifecycleFields contains only bounded values, never a request or protobuf.
// CPU and Memory are present only for the single-container cubebox VM path.
type LifecycleFields struct {
	Metadata              map[string]string
	CPU, Memory           string
	PauseMode, ResumeMode string
	TooLarge              bool
}

func validateFields(f LifecycleFields) error {
	if f.TooLarge || len(f.Metadata) > maxMetadataEntries || len(f.CPU) > 128 || len(f.Memory) > 128 || len(f.PauseMode) > 16 || len(f.ResumeMode) > 16 {
		return ErrCaptureTooLarge
	}
	total := 0
	for k, v := range f.Metadata {
		if len(k) > 256 || len(v) > 4096 {
			return ErrCaptureTooLarge
		}
		total += len(k) + len(v)
		if total > maxMetadataBytes {
			return ErrCaptureTooLarge
		}
	}
	return nil
}
func cloneFields(f LifecycleFields) LifecycleFields {
	if f.Metadata != nil {
		m := make(map[string]string, len(f.Metadata))
		for k, v := range f.Metadata {
			m[strings.Clone(k)] = strings.Clone(v)
		}
		f.Metadata = m
	}
	f.PauseMode = strings.Clone(f.PauseMode)
	f.ResumeMode = strings.Clone(f.ResumeMode)
	f.CPU = strings.Clone(f.CPU)
	f.Memory = strings.Clone(f.Memory)
	return f
}

// UserMetadata must run before template merging or internal label injection.
// Exact internal namespaces/keys are excluded; user keys such as api_token stay.
func UserMetadata(labels map[string]string) *types.WebhookMetadata {
	f := LifecycleFields{Metadata: labels}
	if validateFields(f) != nil {
		return &types.WebhookMetadata{TooLarge: true}
	}
	m := cloneFields(f).Metadata
	for k := range m {
		if k == "host-mount" || k == constants.Caller || strings.HasPrefix(k, "cube.master.") {
			delete(m, k)
		}
	}
	return &types.WebhookMetadata{Values: m}
}

// PrepareLifecycle snapshots existing spec values before cleanup. No parsing or
// I/O occurs here. Old specs have unknown metadata, so labels are never guessed.
func PrepareLifecycle(req *types.CreateCubeSandboxReq) (string, LifecycleFields) {
	if req == nil {
		return "", LifecycleFields{}
	}
	template := req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID]
	f := LifecycleFields{}
	if req.WebhookMetadata != nil {
		f.Metadata = req.WebhookMetadata.Values
		f.TooLarge = req.WebhookMetadata.TooLarge
	}
	if req.InstanceType == "cubebox" && len(req.Containers) == 1 && req.Containers[0] != nil && req.Containers[0].Resources != nil {
		f.CPU = req.Containers[0].Resources.Cpu
		f.Memory = req.Containers[0].Resources.Mem
	}
	if len(template) > maxStringBytes || validateFields(f) != nil {
		return "", LifecycleFields{TooLarge: true}
	}
	return strings.Clone(template), cloneFields(f)
}

func quantityInteger(raw string, memory bool) *int64 {
	q, err := resource.ParseQuantity(raw)
	if err != nil || q.Sign() <= 0 || q.Cmp(resource.MustParse("9223372036854775807")) >= 0 {
		return nil
	}
	r, ok := new(big.Rat).SetString(q.AsDec().String())
	if !ok {
		return nil
	}
	if memory {
		// Cubelet getMbSize uses Quantity.Value (ceil bytes), then integer MiB.
		n := new(big.Int).Quo(r.Num(), r.Denom())
		if new(big.Int).Rem(r.Num(), r.Denom()).Sign() != 0 {
			n.Add(n, big.NewInt(1))
		}
		if !n.IsInt64() {
			return nil
		}
		n.Quo(n, big.NewInt(1024*1024))
		if n.Sign() <= 0 {
			return nil
		}
		v := n.Int64()
		return &v
	}
	if !r.IsInt() || !r.Num().IsInt64() {
		return nil
	}
	v := r.Num().Int64()
	return &v
}
func mapLifecycle(c *CapturedEvent) *ProtocolEventData {
	f := cloneFields(c.input.Fields)
	p := &ProtocolEventData{SandboxMetadata: f.Metadata, PauseMode: f.PauseMode, ResumeMode: f.ResumeMode}
	if c.input.Type == TypePaused {
		cpu, memory := quantityInteger(f.CPU, false), quantityInteger(f.Memory, true)
		if f.CPU != "" && cpu == nil {
			deliveryCounts.WithLabelValues("mapping", "cpu_unavailable").Inc()
		}
		if f.Memory != "" && memory == nil {
			deliveryCounts.WithLabelValues("mapping", "memory_unavailable").Inc()
		}
		if cpu != nil || memory != nil {
			p.Execution = &ExecutionData{VCPUCount: cpu, MemoryMB: memory}
		}
	}
	return p
}

type publisherSlot struct{ publisher Publisher }

// PublisherHolder permits installation after background core initialization.
type PublisherHolder struct{ slot atomic.Pointer[publisherSlot] }

func (h *PublisherHolder) Set(p Publisher) {
	if p == nil {
		p = NopPublisher{}
	}
	h.slot.Store(&publisherSlot{p})
}
func (h *PublisherHolder) Publish(kind, sandbox, template string, f LifecycleFields) {
	s := h.slot.Load()
	if s == nil {
		return
	}
	if _, off := s.publisher.(NopPublisher); off {
		return
	}
	ev, err := Capture(CaptureInput{ID: uuid.New(), Type: kind, Origin: OriginLifecycle, SandboxID: sandbox, TemplateID: template, OccurredAt: time.Now(), Fields: f})
	if err == nil {
		s.publisher.TryPublish(ev)
	}
}

type originKey struct{}

func WithOrigin(ctx context.Context, o Origin) context.Context {
	return context.WithValue(ctx, originKey{}, o)
}
func OriginFromContext(ctx context.Context) Origin {
	if ctx != nil {
		if o, ok := ctx.Value(originKey{}).(Origin); ok {
			return o
		}
	}
	return OriginLifecycle
}

type startKey struct{}
type startSource struct {
	template string
	snapshot bool
}

// WithResolvedStart is called only with the existing trusted template result.
func WithResolvedStart(ctx context.Context, template string, snapshot bool) context.Context {
	return context.WithValue(ctx, startKey{}, startSource{template, snapshot})
}
func ResolvedStart(ctx context.Context) (string, bool) {
	if ctx != nil {
		if s, ok := ctx.Value(startKey{}).(startSource); ok {
			return s.template, s.snapshot
		}
	}
	return "", false
}

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
	EndAtMS               int64
	KillReason            string
	Network               *NetworkFields

	// Template lifecycle fields
	SourceImageRef string
	InstanceType   string
	BuildMode      string
	ArtifactID     string
	ReadyNodeCount int32
	ErrorMsg       string

	// Snapshot lifecycle fields
	NewGen uint32

	// Volume lifecycle fields
	VolumeDriver string
}

func validateFields(f LifecycleFields) error {
	if len(f.KillReason) > maxStringBytes || validateNetwork(f.Network) != nil || f.TooLarge ||
		len(f.Metadata) > maxMetadataEntries ||
		len(f.CPU) > 128 || len(f.Memory) > 128 ||
		len(f.PauseMode) > 16 || len(f.ResumeMode) > 16 ||
		len(f.SourceImageRef) > maxStringBytes || len(f.InstanceType) > 128 ||
		len(f.BuildMode) > 16 || len(f.ArtifactID) > maxStringBytes ||
		len(f.ErrorMsg) > maxStringBytes || len(f.VolumeDriver) > 16 {
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
	f.KillReason = strings.Clone(f.KillReason)
	f.Network = cloneNetwork(f.Network)
	f.CPU = strings.Clone(f.CPU)
	f.Memory = strings.Clone(f.Memory)
	f.SourceImageRef = strings.Clone(f.SourceImageRef)
	f.InstanceType = strings.Clone(f.InstanceType)
	f.BuildMode = strings.Clone(f.BuildMode)
	f.ArtifactID = strings.Clone(f.ArtifactID)
	f.ErrorMsg = strings.Clone(f.ErrorMsg)
	f.VolumeDriver = strings.Clone(f.VolumeDriver)
	return f
}

// IsUserMetadataKey reports whether a label is a sandbox's user metadata
// rather than one Cube sets itself (the caller, the host-mount request, and
// every cube.master.* key).
func IsUserMetadataKey(key string) bool {
	return key != "host-mount" && key != constants.Caller && !strings.HasPrefix(key, "cube.master.")
}

// TemplateLifecycleOpts carries the template build context a subscriber needs
// to correlate build.started with build.completed/failed. Every field is
// optional: a template event is identified by its template id, not by these.
type TemplateLifecycleOpts struct {
	SourceImageRef string
	InstanceType   string
	BuildMode      string
	ArtifactID     string
	ReadyNodeCount int32
	ErrorMsg       string
	NewGen         uint32
	Metadata       map[string]string
}

func PrepareTemplateLifecycle(opts TemplateLifecycleOpts) LifecycleFields {
	f := LifecycleFields{
		SourceImageRef: opts.SourceImageRef,
		InstanceType:   opts.InstanceType,
		BuildMode:      opts.BuildMode,
		ArtifactID:     opts.ArtifactID,
		ReadyNodeCount: opts.ReadyNodeCount,
		ErrorMsg:       opts.ErrorMsg,
		NewGen:         opts.NewGen,
	}
	if opts.Metadata != nil {
		f.Metadata = opts.Metadata
	}
	if validateFields(f) != nil {
		return LifecycleFields{TooLarge: true}
	}
	return cloneFields(f)
}

// PrepareVolumeLifecycle carries the driver a volume was created with. It is
// the only attribute a subscriber needs to route the event, and it cannot be
// recovered from the volume id afterwards.
func PrepareVolumeLifecycle(driver string) LifecycleFields {
	f := LifecycleFields{VolumeDriver: driver}
	if validateFields(f) != nil {
		return LifecycleFields{TooLarge: true}
	}
	return cloneFields(f)
}

// UserMetadata must run before template merging. The request constructor's
// internal labels do not consume the user budget. Exact internal namespaces/keys
// are excluded; user keys such as api_token stay.
func UserMetadata(labels map[string]string) LifecycleFields {
	entries := len(labels)
	for _, key := range []string{constants.Caller, constants.CubeAnnotationsInsType, constants.CubeAnnotationAppSnapshotTemplateID} {
		if _, ok := labels[key]; ok {
			entries--
		}
	}
	// Bound traversal before filtering: at most 64 input user labels and the
	// three known constructor labels, even if other reserved keys are supplied.
	if entries > maxMetadataEntries {
		return LifecycleFields{TooLarge: true}
	}
	var metadata map[string]string
	if labels != nil {
		metadata = make(map[string]string, entries)
		for k, v := range labels {
			if !IsUserMetadataKey(k) {
				continue
			}
			metadata[k] = v
		}
	}
	f := LifecycleFields{Metadata: metadata}
	if validateFields(f) != nil {
		return LifecycleFields{TooLarge: true}
	}
	return cloneFields(f)
}

// PrepareLifecycle snapshots existing spec values before cleanup. No parsing or
// I/O occurs here. Old specs have unknown metadata, so labels are never guessed.
func PrepareLifecycle(req *types.CreateCubeSandboxReq) (string, LifecycleFields) {
	if req == nil {
		return "", LifecycleFields{}
	}
	template := req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID]
	f := LifecycleFields{}
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
	p := &ProtocolEventData{
		SandboxMetadata: f.Metadata,
		PauseMode:       f.PauseMode,
		ResumeMode:      f.ResumeMode,
		SourceImageRef:  f.SourceImageRef,
		InstanceType:    f.InstanceType,
		BuildMode:       f.BuildMode,
		ArtifactID:      f.ArtifactID,
		ReadyNodeCount:  f.ReadyNodeCount,
		ErrorMsg:        f.ErrorMsg,
		NewGen:          f.NewGen,
		VolumeDriver:    f.VolumeDriver,
	}
	if c.input.Type == TypeUpdated {
		if f.EndAtMS > 0 {
			v := time.UnixMilli(f.EndAtMS).UTC().Format(time.RFC3339)
			p.SetTimeout = &v
		}
		p.NetworkEgress = mapNetwork(f.Network)
	}
	p.KillReason = f.KillReason
	if c.input.Type == TypePaused || c.input.Type == TypeKilled {
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
	h.PublishContext(context.Background(), kind, sandbox, template, f)
}

func (h *PublisherHolder) Enabled() bool {
	s := h.slot.Load()
	if s == nil {
		return false
	}
	_, off := s.publisher.(NopPublisher)
	return !off
}
func (h *PublisherHolder) PublishContext(ctx context.Context, kind, sandbox, template string, f LifecycleFields) {
	h.publish(ctx, CaptureInput{Type: kind, SandboxID: sandbox, TemplateID: template, Fields: f})
}

// PublishSnapshotContext publishes a snapshot lifecycle event. Snapshot events
// are identified by the snapshot they describe, not by a template: carrying the
// snapshot id in TemplateID would make snapshot.deleted indistinguishable from
// a template event on the wire.
//
// template may still be set: a snapshot taken from a sandbox started from a
// template is that template's fork, so both ids are actionable.
func (h *PublisherHolder) PublishSnapshotContext(ctx context.Context, kind, sandbox, template, snapshot string, f LifecycleFields) {
	h.publish(ctx, CaptureInput{Type: kind, SandboxID: sandbox, TemplateID: template, SnapshotID: snapshot, Fields: f})
}

// PublishVolumeContext publishes a volume lifecycle event. A volume is its own
// resource: it has no sandbox or template, and its id is the only identity a
// subscriber needs to correlate created with deleted.
func (h *PublisherHolder) PublishVolumeContext(ctx context.Context, kind, volume string, f LifecycleFields) {
	h.publish(ctx, CaptureInput{Type: kind, VolumeID: volume, Fields: f})
}

// publish fills in the identity every event shares. The entry points above
// differ only in which resource ids they carry.
func (h *PublisherHolder) publish(ctx context.Context, in CaptureInput) {
	s := h.slot.Load()
	if s == nil {
		return
	}
	if _, off := s.publisher.(NopPublisher); off {
		return
	}
	in.ID = uuid.New()
	in.Origin = OriginFromContext(ctx)
	in.BuildID = BuildIDFromContext(ctx)
	in.OccurredAt = time.Now()
	ev, err := Capture(in)
	if err == nil {
		s.publisher.TryPublish(ev)
	}
}

type buildIDKey struct{}

// WithBuildID carries a build (job) id to the capture, so template build events
// can be correlated with the job that produced them.
func WithBuildID(ctx context.Context, buildID string) context.Context {
	if strings.TrimSpace(buildID) == "" {
		return ctx
	}
	return context.WithValue(ctx, buildIDKey{}, buildID)
}

// BuildIDFromContext returns the build id attached by WithBuildID, if any.
func BuildIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(buildIDKey{}).(string)
	return id
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

type killReasonKey struct{}

// WithKillReason keeps notification provenance in the existing operation context.
// It never changes the business request, node annotations or retry state.
func WithKillReason(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, killReasonKey{}, reason)
}

func KillReasonFromContext(ctx context.Context) string {
	if ctx != nil {
		if reason, ok := ctx.Value(killReasonKey{}).(string); ok {
			return reason
		}
	}
	return ""
}

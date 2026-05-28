// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package sandboxspec persists the canonical create-time spec of a sandbox.
//
// Motivation: when control-plane flows (e.g. snapshot create) need to act on
// a running sandbox, they previously required the caller to re-supply the
// original CreateCubeSandboxReq. That broke encapsulation and pushed easy
// drift onto the caller. Persisting the canonical spec on create-success and
// removing it on destroy-success gives the control plane a single source of
// truth keyed by sandbox_id.
package sandboxspec

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	// ErrSandboxSpecNotFound is returned when no spec is registered for the sandbox.
	ErrSandboxSpecNotFound = errors.New("sandbox spec not found")
	// ErrSandboxSpecStoreNotReady is returned when Init has not been called.
	ErrSandboxSpecStoreNotReady = errors.New("sandbox spec store is not initialized")
)

var (
	dbMu sync.RWMutex
	db   *gorm.DB
)

// Init wires the shared gorm handle and ensures the underlying table exists.
// It is idempotent and safe to call multiple times.
func Init(client *gorm.DB) error {
	if client == nil {
		return errors.New("sandboxspec.Init: nil db")
	}
	// Schema for t_cube_sandbox_spec is owned by pkg/base/dao/migrate and
	// applied at startup before Init runs; here we only cache the handle.
	dbMu.Lock()
	db = client
	dbMu.Unlock()
	return nil
}

func getDB() *gorm.DB {
	dbMu.RLock()
	defer dbMu.RUnlock()
	return db
}

// IsReady reports whether the store has a usable db handle.
func IsReady() bool {
	return getDB() != nil
}

// PutOptions controls per-record metadata flags. Most callers should
// construct it via PutSandboxSpec; the explicit type makes "backfilled"
// flag impossible to forget.
type PutOptions struct {
	HostID     string
	HostIP     string
	Backfilled bool
}

// Put records (or refreshes) the canonical spec for sandboxID. The request
// is canonicalized before storage to remove transient fields (Timeout, InsId,
// nested Request envelope) so the stored value is deterministic.
//
// The original sandbox creation flow MUST NOT fail if Put fails - callers
// should log and proceed, as the spec store is recovery-friendly: a missing
// record is later detected and backfilled best-effort.
func Put(ctx context.Context, sandboxID string, req *sandboxtypes.CreateCubeSandboxReq, opts PutOptions) error {
	err := doPut(ctx, sandboxID, req, opts)
	if err != nil {
		recordPersistFailure(persistFailureReason(err))
	}
	return err
}

func doPut(ctx context.Context, sandboxID string, req *sandboxtypes.CreateCubeSandboxReq, opts PutOptions) error {
	client := getDB()
	if client == nil {
		return ErrSandboxSpecStoreNotReady
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" || req == nil {
		return errors.New("sandboxspec.Put: sandbox_id and req are required")
	}
	canonical, err := CanonicalizeRequest(req)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(canonical)
	if err != nil {
		return err
	}
	templateID := ""
	if canonical.Annotations != nil {
		templateID = strings.TrimSpace(canonical.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	}
	rec := &models.SandboxSpec{
		SandboxID:    sandboxID,
		TemplateID:   templateID,
		InstanceType: canonical.InstanceType,
		NetworkType:  canonical.NetworkType,
		HostID:       strings.TrimSpace(opts.HostID),
		HostIP:       strings.TrimSpace(opts.HostIP),
		RequestJSON:  string(payload),
		Backfilled:   opts.Backfilled,
	}
	// Single-roundtrip UPSERT keyed on the unique sandbox_id index so the
	// hot create-success path doesn't pay for a SELECT+INSERT/UPDATE pair.
	// gorm.Model's auto-managed id/created_at/updated_at columns are
	// intentionally omitted from DoUpdates so:
	//   - id is preserved on conflict (UNIQUE-key-only conflict path),
	//   - created_at is preserved on conflict (only set on first insert),
	//   - updated_at is automatically refreshed by gorm via UpdateTime hook.
	return client.WithContext(ctx).Table(constants.SandboxSpecTableName).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "sandbox_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"template_id",
				"instance_type",
				"network_type",
				"host_id",
				"host_ip",
				"request_json",
				"backfilled",
				"updated_at",
			}),
		}).
		Create(rec).Error
}

// persistFailureReason classifies a Put error into a low-cardinality metric
// label. Unknown errors collapse to "db_error" so the metric never explodes
// in cardinality on transient gorm/network errors.
func persistFailureReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrSandboxSpecStoreNotReady):
		return "store_not_ready"
	case strings.Contains(err.Error(), "sandbox_id and req are required"):
		return "invalid_input"
	case strings.Contains(err.Error(), "json"):
		return "marshal_error"
	default:
		return "db_error"
	}
}

// Get returns a deep copy of the canonical request for sandboxID.
func Get(ctx context.Context, sandboxID string) (*sandboxtypes.CreateCubeSandboxReq, error) {
	client := getDB()
	if client == nil {
		return nil, ErrSandboxSpecStoreNotReady
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil, errors.New("sandboxspec.Get: sandbox_id is required")
	}
	var rec models.SandboxSpec
	if err := client.WithContext(ctx).Table(constants.SandboxSpecTableName).
		Where("sandbox_id = ?", sandboxID).First(&rec).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSandboxSpecNotFound
		}
		return nil, err
	}
	out := &sandboxtypes.CreateCubeSandboxReq{}
	if err := json.Unmarshal([]byte(rec.RequestJSON), out); err != nil {
		return nil, err
	}
	return out, nil
}

// Delete removes the spec record for sandboxID. Missing records are a no-op.
func Delete(ctx context.Context, sandboxID string) error {
	client := getDB()
	if client == nil {
		return ErrSandboxSpecStoreNotReady
	}
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return errors.New("sandboxspec.Delete: sandbox_id is required")
	}
	return client.WithContext(ctx).Table(constants.SandboxSpecTableName).
		Where("sandbox_id = ?", sandboxID).
		Delete(&models.SandboxSpec{}).Error
}

// CanonicalizeRequest strips transient fields that are not part of the
// long-term spec (per-call timeouts, debug host pinning, request envelope).
// Annotation/label maps are forced to non-nil so JSON encoding is stable.
//
// SnapshotDir is preserved: it influences how cubelet persists snapshot
// metadata and downstream snapshot-create flows need it to round-trip.
//
// v4: per-invocation runtime snapshot/AppSnapshot physical-binding annotations
// are stripped before persisting. These are populated on the wire when the
// caller asks to restore-from-snapshot or restore-from-template (cubelet is
// the authority for the underlying physical refs); they MUST NOT bleed into
// later snapshots taken of the resulting sandbox or into stored template
// requests, otherwise the next snapshot would carry a stale physical binding
// that no longer matches the new sandbox's catalog entry.
func CanonicalizeRequest(req *sandboxtypes.CreateCubeSandboxReq) (*sandboxtypes.CreateCubeSandboxReq, error) {
	if req == nil {
		return nil, errors.New("nil req")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	out := &sandboxtypes.CreateCubeSandboxReq{}
	if err := json.Unmarshal(payload, out); err != nil {
		return nil, err
	}
	if out.Annotations == nil {
		out.Annotations = map[string]string{}
	}
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	out.Timeout = 0
	out.InsId = ""
	out.InsIp = ""
	out.Request = nil
	stripTransientSnapshotAnnotations(out.Annotations)
	return out, nil
}

// stripTransientSnapshotAnnotations removes runtime-only snapshot binding
// annotations from a canonicalized request. Logical id markers
// (CubeAnnotationAppSnapshotTemplateID) are intentionally retained because
// they describe the long-term provenance of the sandbox; only per-invocation
// transient binding values are stripped.
//
// v5: physical memory volume annotations (CubeAnnotation{App,Runtime}Snapshot*MemoryVol/MemoryKind/MemoryDev)
// no longer exist as constants — Cubelet's local snapshot catalog is the
// single source of truth for physical refs and is keyed by logical id.
func stripTransientSnapshotAnnotations(annotations map[string]string) {
	if annotations == nil {
		return
	}
	delete(annotations, constants.CubeAnnotationRuntimeSnapshotID)
	delete(annotations, constants.CubeAnnotationRuntimeSnapshotAttachedAt)
}

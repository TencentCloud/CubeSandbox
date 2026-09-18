// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"

	"github.com/tencentcloud/CubeSandbox/OpsAgent/internal/upstream"
	"github.com/tencentcloud/CubeSandbox/OpsAgent/pkg/atomicfile"
)

// pushRequest mirrors CubeOps' OpsAgentPushRequest.
type pushRequest struct {
	RequestID string         `json:"request_id"`
	Revision  int64          `json:"revision"`
	Spec      *Spec          `json:"spec"`
	Physical  PhysicalTotals `json:"physical"`
	// OnlyPausedRatio restricts the push to the ratio field (cluster-managed
	// rows are authoritative for the ratio alone).
	OnlyPausedRatio bool `json:"only_paused_ratio,omitempty"`
}

type pushResponse struct {
	RequestID string `json:"request_id"`
	Applied   bool   `json:"applied"`
	Noop      bool   `json:"noop,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Puller abstracts the CubeOps pull endpoint for tests.
type Puller interface {
	PullSpec(ctx context.Context, nodeID string) (*upstream.SpecResponse, error)
}

// Domain implements the quota task domain.
type Domain struct {
	nodeID          string
	dynamicconfPath string
	backupKeep      int
	puller          Puller
	// physicalFn resolves the host capacity used by the overcommit guards.
	// Defaults to hostPhysical; tests inject a fixed value for determinism.
	physicalFn func() (PhysicalTotals, error)

	mu sync.Mutex // serializes apply (push + reconcile)
	// lastApplied{Revision,At}: skip lower revision with older/equal updated_at; updated_at alone self-heals server-side revision-space reset.
	lastAppliedRevision int64
	lastAppliedAt       time.Time
}

// New creates the quota domain for one node.
func New(nodeID, dynamicconfPath string, backupKeep int, puller Puller) *Domain {
	return &Domain{
		nodeID:          nodeID,
		dynamicconfPath: dynamicconfPath,
		backupKeep:      backupKeep,
		puller:          puller,
		physicalFn:      hostPhysical,
	}
}

func (d *Domain) Name() string { return "quota" }

func (d *Domain) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"POST /api/v1/config/quota": d.handlePush,
	}
}

// handlePush applies a spec synchronously: validate → no-op check → backup →
// atomic write → read-back. applied=true only after verification; a rejected
// spec answers 200 with error detail (control-plane decision, not transport).
func (d *Domain) handlePush(w http.ResponseWriter, r *http.Request) {
	var req pushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, pushResponse{Error: "invalid body: " + err.Error()})
		return
	}
	if req.Spec == nil {
		writeJSON(w, http.StatusBadRequest, pushResponse{RequestID: req.RequestID, Error: "spec is required"})
		return
	}

	resp := pushResponse{RequestID: req.RequestID}
	req.Spec.OnlyRatio = req.OnlyPausedRatio
	noop, err := d.apply(req.Spec, req.Revision, req.Physical)
	switch {
	case errors.Is(err, ErrValidation):
		// Control-plane decision: a rejected spec, not a transport failure.
		cubelog.Infof("quota: push rejected: request=%s err=%v", req.RequestID, err)
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	case err != nil:
		// Node-side I/O failure: surface as 5xx so observers don't read it as success.
		cubelog.Errorf("quota: push failed: request=%s err=%v", req.RequestID, err)
		resp.Error = err.Error()
		writeJSON(w, http.StatusInternalServerError, resp)
		return
	case noop:
		resp.Applied, resp.Noop = true, true
	default:
		resp.Applied = true
	}

	cubelog.Infof("quota: push applied: request=%s revision=%d noop=%v", req.RequestID, req.Revision, resp.Noop)
	writeJSON(w, http.StatusOK, resp)
}

// apply merges the spec into dynamicconf (noop when matching), guarding
// overcommit with local host capacity; (revision, updated_at) ordering skips stale payloads to prevent regression.
func (d *Domain) apply(spec *Spec, revision int64, _ PhysicalTotals) (noop bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Skip stale payloads (lower revision, older/equal updated_at); a fresh updated_at self-heals a revision-space reset.
	if revision < d.lastAppliedRevision && !spec.UpdatedAt.IsZero() && !spec.UpdatedAt.After(d.lastAppliedAt) {
		cubelog.Infof("quota: stale apply skipped: revision=%d updated_at=%s < lastApplied=%d/%s",
			revision, spec.UpdatedAt.Format(time.RFC3339), d.lastAppliedRevision, d.lastAppliedAt.Format(time.RFC3339))
		return true, nil
	}

	physical, err := d.physicalFn()
	if err != nil {
		return false, fmt.Errorf("resolve host capacity: %w", err)
	}
	if err := spec.Validate(physical); err != nil {
		return false, err
	}
	current, err := os.ReadFile(d.dynamicconfPath)
	if err != nil {
		return false, fmt.Errorf("read dynamicconf: %w", err)
	}
	if matches, err := Matches(string(current), spec); err != nil {
		return false, fmt.Errorf("inspect dynamicconf: %w", err)
	} else if matches {
		d.lastAppliedRevision = revision
		return true, nil
	}
	merged, err := Merge(string(current), spec)
	if err != nil {
		return false, err
	}
	if _, err := atomicfile.Backup(d.dynamicconfPath, d.backupKeep); err != nil {
		return false, fmt.Errorf("backup dynamicconf: %w", err)
	}
	if err := atomicfile.Replace(d.dynamicconfPath, merged); err != nil {
		return false, fmt.Errorf("write dynamicconf: %w", err)
	}
	after, err := os.ReadFile(d.dynamicconfPath)
	if err != nil {
		return false, fmt.Errorf("read back dynamicconf: %w", err)
	}
	if ok, err := Matches(string(after), spec); err != nil || !ok {
		return false, fmt.Errorf("read-back mismatch after write (ok=%v err=%v)", ok, err)
	}
	d.lastAppliedRevision, d.lastAppliedAt = revision, spec.UpdatedAt
	return false, nil
}

// Reconcile pulls the authoritative spec on every tick through the push
// path: matching files no-op, drifted ones heal regardless of revision.
func (d *Domain) Reconcile(ctx context.Context) {
	resp, err := d.puller.PullSpec(ctx, d.nodeID)
	if err != nil {
		cubelog.Warnf("quota: reconcile pull failed: %v", err)
		return
	}
	if !resp.Managed || resp.Spec == nil {
		return
	}
	spec := convertSpec(resp.Spec)
	if resp.OnlyPausedRatio {
		spec.OnlyRatio = true
	}
	noop, err := d.apply(spec, resp.Revision, PhysicalTotals{})
	if err != nil {
		cubelog.Errorf("quota: reconcile apply failed: revision=%d err=%v", resp.Revision, err)
		return
	}
	if noop {
		return
	}
	cubelog.Infof("quota: reconcile applied revision=%d", resp.Revision)
}

func convertSpec(s *upstream.Spec) *Spec {
	return &Spec{
		MCpuLimit:                  s.MCpuLimit,
		MemLimit:                   s.MemLimit,
		MvmLimit:                   s.MvmLimit,
		CreationConcurrentNum:      s.CreationConcurrentNum,
		PausedResourceReleaseRatio: s.PausedResourceReleaseRatio,
		UpdatedAt:                  s.UpdatedAt,
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

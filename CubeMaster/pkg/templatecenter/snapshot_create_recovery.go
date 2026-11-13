// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SnapshotCreateRecovery is an operator attestation that the old node has been
// fenced outside the control plane. A missing heartbeat is not such evidence.
type SnapshotCreateRecovery struct {
	SandboxID         string `json:"sandbox_id"`
	NodeID            string `json:"node_id"`
	NodeIP            string `json:"node_ip"`
	ConfirmNodeFenced bool   `json:"confirm_node_fenced"`
	Reason            string `json:"reason"`
}

// RecoverSnapshotCreateReservation releases only a stale create_pending binding.
// Ordinary live bindings are never affected. The retained history records why an
// operator overrode the absence of an authoritative node inventory.
func RecoverSnapshotCreateReservation(ctx context.Context, snapshotID string, req SnapshotCreateRecovery) error {
	if !isReady() {
		return ErrTemplateStoreNotInitialized
	}
	snapshotID = strings.TrimSpace(snapshotID)
	req.SandboxID, req.NodeID, req.NodeIP = strings.TrimSpace(req.SandboxID), strings.TrimSpace(req.NodeID), strings.TrimSpace(req.NodeIP)
	req.Reason = strings.TrimSpace(req.Reason)
	if snapshotID == "" || req.SandboxID == "" || (req.NodeID == "" && req.NodeIP == "") || !req.ConfirmNodeFenced || req.Reason == "" || len(req.Reason) > 1024 {
		return errors.New("snapshot, sandbox, node identity, confirm_node_fenced and a reason of at most 1024 bytes are required")
	}
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		rec, err := lockSnapshotRecordTx(tx, snapshotID)
		if err != nil {
			return err
		}
		if !strings.EqualFold(rec.Status, StatusDeleted) {
			return fmt.Errorf("%w: snapshot must be retired before recovery", ErrTemplateInUse)
		}
		if err := rejectActiveSnapshotJobTx(tx, snapshotID); err != nil {
			return err
		}
		var ref models.SnapshotRuntimeActive
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("sandbox_id = ? AND binding_type = ?", req.SandboxID, SnapshotRuntimeBindingCreatePending).First(&ref).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if ref.SnapshotID != snapshotID || ref.NodeID != req.NodeID || ref.NodeIP != req.NodeIP {
			return fmt.Errorf("%w: reservation identity does not match", ErrTemplateInUse)
		}
		now := time.Now()
		if ref.AttachedAt.IsZero() || now.Sub(ref.AttachedAt) < snapshotOrphanRuntimeRefTTL {
			return fmt.Errorf("%w: reservation must be at least 24 hours old", ErrTemplateInUse)
		}
		if sandboxExistsOnMasterFn(ctx, ref.SandboxID) {
			return fmt.Errorf("%w: sandbox still exists on master", ErrTemplateInUse)
		}
		for _, n := range getSnapshotReconcilerNodes(-1, cubeboxv1.InstanceType_cubebox.String()) {
			if (ref.NodeID != "" && n.ID() == ref.NodeID) || (ref.NodeIP != "" && n.HostIP() == ref.NodeIP) {
				return fmt.Errorf("%w: node is healthy; reconcile its inventory instead", ErrTemplateInUse)
			}
		}
		return detachObservedSnapshotRuntimeBindingTx(tx, ref, "operator confirmed node fenced: "+req.Reason, now)
	})
}

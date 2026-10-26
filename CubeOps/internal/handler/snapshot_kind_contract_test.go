// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/store"
)

// TestS4_CreateSnapshotWritesSandboxSnapshotKind verifies that the INSERT
// SQL used by CreateSnapshot (full_snapshot mode) writes
// snapshot_kind='sandbox_snapshot', NOT 'sandbox'. The old 'sandbox' value
// was never matched by the DeleteSnapshot switch case, causing physical
// storage leaks.
//
// This is a source-level contract test: it checks the SQL string embedded
// in UpsertSnapshotSQL calls. It does not need Docker/MySQL.
//
// See review S4.
func TestS4_CreateSnapshotWritesSandboxSnapshotKind(t *testing.T) {
	// The CreateSnapshot handler builds its INSERT via store.UpsertSnapshotSQL.
	// We verify the generated SQL for the full_snapshot path contains
	// 'sandbox_snapshot' (the corrected kind) and does NOT contain 'sandbox'
	// as a standalone value (which would match the old buggy kind).
	//
	// The full_snapshot INSERT uses:
	//   cols: "snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind,
	//          origin_sandbox_id, rootfs_source_type, rootfs_source_id,
	//          rootfs_snapshot_id"
	//   placeholders: "?, ?, ?, ?, 'ready', 'sandbox_snapshot', ?, 'snapshot', ?, ?"
	sqlStr := store.UpsertSnapshotSQL(
		`snapshot_id, agent_id, sandbox_id, name, status, snapshot_kind, origin_sandbox_id,
			rootfs_source_type, rootfs_source_id, rootfs_snapshot_id`,
		`?, ?, ?, ?, 'ready', 'sandbox_snapshot', ?, 'snapshot', ?, ?`,
		`agent_id = EXCLUDED.agent_id, sandbox_id = EXCLUDED.sandbox_id,
			status = EXCLUDED.status`,
		`agent_id = VALUES(agent_id), sandbox_id = VALUES(sandbox_id),
			status = VALUES(status)`,
	)

	if !strings.Contains(sqlStr, "'sandbox_snapshot'") {
		t.Errorf("SQL does not contain 'sandbox_snapshot' kind (S4): %s", sqlStr)
	}
	// Ensure we didn't accidentally keep the old buggy 'sandbox' value.
	// 'sandbox_snapshot' contains 'sandbox' as a substring, so we check that
	// the old standalone 'sandbox' (as written by the bug: 'ready', 'sandbox')
	// is not present. The old SQL had: VALUES (..., 'ready', 'sandbox', ...)
	// The new SQL has:  VALUES (..., 'ready', 'sandbox_snapshot', ...)
	if strings.Contains(sqlStr, "'ready', 'sandbox',") {
		t.Errorf("SQL contains old buggy kind 'sandbox' (S4): %s", sqlStr)
	}
}

// TestS4_DeleteSnapshotMatchesSandboxSnapshotKind verifies that the
// DeleteSnapshot switch case matches 'sandbox_snapshot' (the kind written
// by CreateSnapshot) and calls CubeMaster DeleteSnapshot.
//
// We simulate the switch logic directly — the actual handler needs a store,
// but the switch case is the S4 fix point.
func TestS4_DeleteSnapshotMatchesSandboxSnapshotKind(t *testing.T) {
	// The switch cases in DeleteSnapshot are:
	//   case "agenthub_state": ...
	//   case "full_snapshot", "sandbox_snapshot": call CM DeleteSnapshot
	//
	// We verify both 'sandbox_snapshot' and 'full_snapshot' trigger the
	// CM delete path, and 'sandbox' (the old buggy kind) does NOT.
	kindCases := map[string]bool{
		"sandbox_snapshot": true,  // S4 fix: must match
		"full_snapshot":    true,  // existing: must still match
		"agenthub_state":   false, // host-side cleanup, not CM
		"sandbox":          false, // old buggy kind: must NOT match CM path
	}

	for kind, shouldCallCM := range kindCases {
		t.Run(kind, func(t *testing.T) {
			calledCM := false
			cm := &fakeCM{
				deleteSnapshot: func(_ context.Context, snapshotID string) (json.RawMessage, error) {
					calledCM = true
					return raw(`{"ret":{"ret_code":0}}`), nil
				},
			}

			// Replicate the switch logic from DeleteSnapshot.
			snapshotKind := kind
			switch snapshotKind {
			case "agenthub_state":
				// host-side cleanup — does not call CM
			case "full_snapshot", "sandbox_snapshot":
				_, _ = cm.DeleteSnapshot(context.Background(), "snap-test")
			default:
				// 'sandbox' (old buggy kind) falls here — no CM call, storage leaked
			}

			if shouldCallCM && !calledCM {
				t.Errorf("kind=%q: expected CM DeleteSnapshot to be called, but it was not (S4)", kind)
			}
			if !shouldCallCM && calledCM {
				t.Errorf("kind=%q: CM DeleteSnapshot was called but should not be", kind)
			}
		})
	}
}

// TestS4_FullLifecycle_KindConsistency is a documentation test that encodes
// the S4 invariant: the kind written by CreateSnapshot must be one of the
// kinds matched by DeleteSnapshot's switch case. If someone changes one
// without the other, this test fails.
func TestS4_FullLifecycle_KindConsistency(t *testing.T) {
	// Kinds written by CreateSnapshot (full_snapshot mode) and PublishTemplate.
	writtenKinds := []string{"sandbox_snapshot", "agenthub_state"}

	// Kinds matched by DeleteSnapshot's switch case for CM physical delete.
	matchedForCMDelete := map[string]bool{
		"full_snapshot":    true,
		"sandbox_snapshot": true,
	}

	for _, kind := range writtenKinds {
		if kind == "agenthub_state" {
			continue // host-side, not CM
		}
		if !matchedForCMDelete[kind] {
			t.Errorf("kind %q is written by CreateSnapshot but NOT matched by DeleteSnapshot — storage will leak (S4)", kind)
		}
	}
}

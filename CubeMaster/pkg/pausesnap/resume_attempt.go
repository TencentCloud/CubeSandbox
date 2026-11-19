// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package pausesnap

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"gorm.io/gorm"
)

// Resume attempt states on a pause binding.
const (
	// ResumeInflight: a Resume was sent to ResumeTarget and its outcome is
	// unknown. The sandbox may be starting or running there.
	ResumeInflight = "inflight"
	// ResumeFailed: ResumeTarget answered that its attempt failed, so the
	// binding may be resumed anywhere again.
	ResumeFailed = "failed"
)

// ErrResumePinned is returned when another node holds the binding's
// unresolved Resume attempt.
var ErrResumePinned = errors.New("pause binding has an unresolved resume attempt on another node")

// PinnedResumeTarget returns the node a Resume of rec must go to, or "" when
// any node may be chosen. Only an attempt whose outcome is unknown pins.
func PinnedResumeTarget(rec *Record) string {
	if rec == nil || rec.ResumeState != ResumeInflight {
		return ""
	}
	return strings.TrimSpace(rec.ResumeTarget)
}

// ResumeAttempt is one Resume of a binding sent to Target. N is the value of
// resume_attempt it wrote: it fences the attempt, so only the Master that
// sent it can resolve it, never one acting on an older attempt.
type ResumeAttempt struct {
	Target string
	N      int
	// Fresh is true when no attempt was unresolved before this one: the pin
	// exists only because of it.
	Fresh bool
}

// BeginResumeAttempt records that a Resume of snapshotID is about to be sent
// to target. It is conditional on the row it read: it succeeds only while no
// other node holds an unresolved attempt and nobody else recorded one in the
// meantime, so two Masters (or a retry after a lost reply) cannot send the
// same binding to two nodes.
func BeginResumeAttempt(ctx context.Context, snapshotID, target string) (*ResumeAttempt, error) {
	client := getDB()
	if client == nil {
		return nil, ErrNotReady
	}
	snapshotID = strings.TrimSpace(snapshotID)
	target = strings.TrimSpace(target)
	if snapshotID == "" || target == "" {
		return nil, errors.New("snapshotID and target are required")
	}
	for i := 0; i < beginResumeTries; i++ {
		rec, err := GetBySnapshotID(ctx, snapshotID)
		if err != nil {
			return nil, err
		}
		if pinned := PinnedResumeTarget(rec); pinned != "" && pinned != target {
			return nil, fmt.Errorf("%w: %s", ErrResumePinned, pinned)
		}
		next := rec.ResumeAttempt + 1
		res := client.WithContext(context.WithoutCancel(ctx)).Table(constants.PauseSnapshotTableName).
			Where("snapshot_id = ? AND resume_attempt = ? AND resume_state = ? AND resume_target = ?",
				snapshotID, rec.ResumeAttempt, rec.ResumeState, rec.ResumeTarget).
			Updates(map[string]any{
				"resume_target":  target,
				"resume_state":   ResumeInflight,
				"resume_attempt": next,
				"updated_at":     gorm.Expr("CURRENT_TIMESTAMP"),
			})
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected > 0 {
			return &ResumeAttempt{Target: target, N: next, Fresh: rec.ResumeState != ResumeInflight}, nil
		}
		// Someone recorded an attempt between the read and the write: look
		// again.
	}
	return nil, fmt.Errorf("record resume attempt of %s: the binding keeps changing", snapshotID)
}

const beginResumeTries = 3

// FailResumeAttempt releases the pin of attempt after its target answered
// that it failed. An attempt recorded after it, on whatever node, is left
// alone.
func FailResumeAttempt(ctx context.Context, snapshotID string, attempt *ResumeAttempt) error {
	client := getDB()
	if client == nil {
		return ErrNotReady
	}
	if attempt == nil {
		return errors.New("no resume attempt to release")
	}
	return client.WithContext(context.WithoutCancel(ctx)).Table(constants.PauseSnapshotTableName).
		Where("snapshot_id = ? AND resume_target = ? AND resume_state = ? AND resume_attempt = ?",
			strings.TrimSpace(snapshotID), strings.TrimSpace(attempt.Target), ResumeInflight, attempt.N).
		Updates(map[string]any{
			"resume_state": ResumeFailed,
			"updated_at":   gorm.Expr("CURRENT_TIMESTAMP"),
		}).Error
}

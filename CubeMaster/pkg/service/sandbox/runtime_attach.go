// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/pausesnap"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxlock"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/sandboxspec"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/admission"
	"github.com/tencentcloud/CubeSandbox/pkgs/cubedb/dao"
	"gorm.io/gorm"
)

const (
	runtimeAttachWorkTimeout   = 45 * time.Second
	runtimeAttachSettleTimeout = 2 * time.Second
)

var (
	ErrRuntimeAttachUnresolved = errors.New("runtime attach outcome is unresolved")
	ErrRuntimeAttachRejected   = errors.New("runtime attach did not publish a mount")
	ErrRuntimeAttachConflict   = errors.New("runtime attach request ID has a different binding")
)

// RuntimeAttachRequest is bound to the execution selected by the internal caller.
type RuntimeAttachRequest struct {
	SandboxID     string
	RequestID     string
	ExecutionID   string
	VolumeID      string
	ContainerPath string
	Readonly      bool
}

type RuntimeAttachBinding struct {
	RuntimeAttachRequest
	VolumeRecordID uint
}

type RuntimeAttachOutcome uint8

const (
	RuntimeAttachUnknown RuntimeAttachOutcome = iota
	RuntimeAttachPublished
	RuntimeAttachNotStarted
	RuntimeAttachRolledBack
)

// RuntimeAttachReply describes the original binding. Published requires proof
// of the requested mount on the current execution, and RolledBack requires
// proof that all resources created by the operation have been cleaned up.
type RuntimeAttachReply struct {
	Binding RuntimeAttachBinding
	Outcome RuntimeAttachOutcome
	ExtInfo map[string][]byte
}

// AttachRuntimeVolume coordinates an internal Attach with other lifecycle work.
// The caller supplies a Snapshot check run in the reservation transaction and
// a trusted node dispatcher.
// Dispatch must honor its context and verify the live execution under the node
// lifecycle lock before attempting or reporting publication.
func AttachRuntimeVolume(
	ctx context.Context,
	req RuntimeAttachRequest,
	checkSnapshot func(*gorm.DB, string) error,
	dispatch func(context.Context, RuntimeAttachBinding, models.VolumeRecord) (RuntimeAttachReply, error),
) error {
	if !validRuntimeAttachID(req.SandboxID, 128) || !validRuntimeAttachID(req.VolumeID, 128) ||
		!validRuntimeAttachID(req.RequestID, 256) || !validRuntimeAttachID(req.ExecutionID, 256) {
		return errors.New("runtime attach requires canonical sandbox, request, execution and volume IDs")
	}
	if !path.IsAbs(req.ContainerPath) || path.Clean(req.ContainerPath) != req.ContainerPath ||
		req.ContainerPath == "/" || !validRuntimeAttachText(req.ContainerPath, 4096) {
		return errors.New("runtime attach requires a canonical absolute non-root container path")
	}
	db := dao.Default()
	if found, err := replayRuntimeAttach(ctx, db, req); found || err != nil {
		return err
	}
	if checkSnapshot == nil || dispatch == nil {
		return errors.New("runtime attach requires a snapshot check and node dispatcher")
	}

	return sandboxlock.WithLock(ctx, req.SandboxID, sandboxlock.Options{
		Value: "volume-attach",
		TTL:   sandboxlock.LifecycleTTL,
	}, func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeAttachWorkTimeout)
		defer cancel()

		if found, err := replayRuntimeAttach(workCtx, db, req); found || err != nil {
			return err
		}
		if err := sandboxspec.EnsureNoPendingRuntimeAttach(workCtx, req.SandboxID); err != nil {
			return replayRuntimeAttachAfterAdmissionError(workCtx, db, req, err)
		}
		stored, err := sandboxspec.GetWithRuntimeVolumeMounts(workCtx, req.SandboxID)
		if err != nil {
			return replayRuntimeAttachAfterAdmissionError(workCtx, db, req, err)
		}
		if stored.SandboxID != req.SandboxID {
			return sandboxspec.ErrSandboxSpecNotFound
		}
		request := models.RuntimeVolumeAttach{
			SandboxID: req.SandboxID, SpecRowID: stored.RowID, RequestID: req.RequestID, ExecutionID: req.ExecutionID,
			VolumeID: req.VolumeID, ContainerPath: req.ContainerPath, Readonly: req.Readonly,
		}
		_, volume, err := admission.BeginRuntimeAttach(workCtx, db, request, func(tx *gorm.DB, sandboxID string) error {
			if err := pausesnap.EnsureNoBindingInTx(tx, sandboxID); err != nil {
				return err
			}
			if err := checkSnapshot(tx, sandboxID); err != nil {
				return err
			}
			lockedSpec, err := sandboxspec.GetWithRuntimeVolumeMountsInTx(tx, sandboxID)
			if err != nil {
				return err
			}
			if lockedSpec.SandboxID != sandboxID || lockedSpec.RowID != stored.RowID {
				return sandboxspec.ErrSandboxSpecNotFound
			}
			return validateRuntimeAttachTarget(workCtx, req, lockedSpec)
		})
		if err != nil {
			return replayRuntimeAttachAfterAdmissionError(workCtx, db, req, err)
		}

		if err := workCtx.Err(); err != nil {
			return fmt.Errorf("%w: %w", ErrRuntimeAttachUnresolved, err)
		}
		binding := RuntimeAttachBinding{RuntimeAttachRequest: req, VolumeRecordID: volume.ID}
		reply, err := dispatch(workCtx, binding, volume)
		if err != nil || reply.Binding != binding {
			return ErrRuntimeAttachUnresolved
		}
		return settleRuntimeAttach(ctx, db, req, reply)
	})
}

func validRuntimeAttachID(id string, maxBytes int) bool {
	return id != "" && id == strings.TrimSpace(id) && validRuntimeAttachText(id, maxBytes)
}

func validRuntimeAttachText(text string, maxBytes int) bool {
	return len(text) <= maxBytes && utf8.ValidString(text) && !strings.ContainsRune(text, 0)
}

func getRuntimeAttachRequest(ctx context.Context, db *gorm.DB, req RuntimeAttachRequest) (*models.RuntimeVolumeAttach, error) {
	var record models.RuntimeVolumeAttach
	err := db.WithContext(ctx).Where("request_key = ?", models.RuntimeVolumeAttachKey(req.SandboxID, req.RequestID)).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	persisted := RuntimeAttachRequest{
		SandboxID: record.SandboxID, RequestID: record.RequestID, ExecutionID: record.ExecutionID,
		VolumeID: record.VolumeID, ContainerPath: record.ContainerPath, Readonly: record.Readonly,
	}
	if req != persisted {
		return nil, ErrRuntimeAttachConflict
	}
	return &record, nil
}

func runtimeAttachResult(outcome string) error {
	switch outcome {
	case models.RuntimeAttachPublished:
		return nil
	case models.RuntimeAttachNotStarted, models.RuntimeAttachRolledBack:
		return ErrRuntimeAttachRejected
	default:
		return ErrRuntimeAttachUnresolved
	}
}

func replayRuntimeAttach(ctx context.Context, db *gorm.DB, req RuntimeAttachRequest) (bool, error) {
	record, err := getRuntimeAttachRequest(ctx, db, req)
	if err != nil || record == nil {
		return false, err
	}
	return true, runtimeAttachResult(record.Outcome)
}

func replayRuntimeAttachAfterAdmissionError(ctx context.Context, db *gorm.DB, req RuntimeAttachRequest, admissionErr error) error {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeAttachSettleTimeout)
	defer cancel()
	if found, err := replayRuntimeAttach(readCtx, db, req); found || err != nil {
		return err
	}
	return admissionErr
}

// settleRuntimeAttach accepts only a verified result for the originally bound
// execution. A missing or unknown observation cannot release the reservation.
func settleRuntimeAttach(ctx context.Context, db *gorm.DB, req RuntimeAttachRequest, reply RuntimeAttachReply) error {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeAttachSettleTimeout)
	defer cancel()
	record, err := getRuntimeAttachRequest(settleCtx, db, req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRuntimeAttachUnresolved, err)
	}
	if record == nil || reply.Binding != (RuntimeAttachBinding{RuntimeAttachRequest: req, VolumeRecordID: record.VolumeRecordID}) {
		return ErrRuntimeAttachUnresolved
	}
	if record.Outcome != models.RuntimeAttachPending {
		return runtimeAttachResult(record.Outcome)
	}
	var outcome string
	switch reply.Outcome {
	case RuntimeAttachPublished:
		outcome = models.RuntimeAttachPublished
	case RuntimeAttachNotStarted:
		outcome = models.RuntimeAttachNotStarted
	case RuntimeAttachRolledBack:
		outcome = models.RuntimeAttachRolledBack
	default:
		return ErrRuntimeAttachUnresolved
	}
	if err := admission.SettleRuntimeAttach(settleCtx, db, *record, reply.ExtInfo, outcome); err != nil {
		readCtx, readCancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeAttachSettleTimeout)
		defer readCancel()
		committed, readErr := getRuntimeAttachRequest(readCtx, db, req)
		if readErr == nil && committed != nil && committed.Outcome != models.RuntimeAttachPending {
			return runtimeAttachResult(committed.Outcome)
		}
		return fmt.Errorf("%w: %w", ErrRuntimeAttachUnresolved, errors.Join(err, readErr))
	}
	return runtimeAttachResult(outcome)
}

func validateRuntimeAttachTarget(ctx context.Context, req RuntimeAttachRequest, stored *sandboxspec.SpecWithRuntimeVolumeMounts) error {
	if err := injectPluginVolumeMounts(ctx, stored.Request); err != nil {
		return err
	}
	for _, volume := range stored.Request.Volumes {
		if volume != nil && volume.Name == req.VolumeID {
			return fmt.Errorf("volume %q is already declared on this sandbox", req.VolumeID)
		}
	}
	for _, mount := range stored.Mounts {
		if err := checkRuntimeAttachOverlap(req, mount.VolumeID, mount.ContainerPath); err != nil {
			return err
		}
	}
	for _, container := range stored.Request.Containers {
		if container == nil {
			continue
		}
		for _, mount := range container.VolumeMounts {
			if err := checkRuntimeAttachOverlap(req, mount.GetName(), mount.GetContainerPath()); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkRuntimeAttachOverlap(req RuntimeAttachRequest, volumeID, mountPath string) error {
	if volumeID == req.VolumeID {
		return fmt.Errorf("volume %q is already mounted on this sandbox", req.VolumeID)
	}
	target := path.Clean(mountPath)
	if path.IsAbs(target) && target != "/" && (target == req.ContainerPath ||
		strings.HasPrefix(target, req.ContainerPath+"/") || strings.HasPrefix(req.ContainerPath, target+"/")) {
		return fmt.Errorf("runtime mount path %q conflicts with existing mount %q", req.ContainerPath, target)
	}
	return nil
}

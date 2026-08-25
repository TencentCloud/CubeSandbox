// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

type s3UploadEntry struct {
	state   string
	message string
	uuids   *cow.RemoteUUIDs
}

type cowSnapshotExporter interface {
	ExportSnapshot(name string) (string, error)
}

type cowVolumeImporter interface {
	ImportLvol(name, remoteUUID string) (string, error)
}

// Upload implements [cow.Uploader]. Calls cubecow_export_snapshot per
// snapshot disk (rootfs / memory / metadata). Failure returns an error and
// does not persist remote_uuids — Master treats that as upload failed.
// Success returns real export uuids and records local state as running;
// terminal ready comes later from cubecow export_status via UploadStatus.
func (m *S3Cow) Upload(ctx context.Context, snapshotID string) (*cow.RemoteUUIDs, error) {
	id := strings.TrimSpace(snapshotID)
	if id == "" {
		return nil, fmt.Errorf("snapshot_id is required")
	}
	uuids := &cow.RemoteUUIDs{}
	var exportable []CowObjectRef
	for _, ref := range activateObjectRefs(ctx, cow.BackendS3, id) {
		if IsS3MetadataBaseName(ref.Name) {
			continue
		}
		if ref.Role == "metadata" {
			info, infoErr := m.GetVolumeInfo(ctx, ref.Name)
			exists, existsErr := cowObjectPresent(info, infoErr)
			if existsErr != nil {
				m.setUpload(id, cow.RemoteStateFailed, existsErr.Error(), nil)
				return nil, fmt.Errorf("upload %s %s: %w", ref.Role, ref.Name, existsErr)
			}
			if !exists {
				continue
			}
		}
		exportable = append(exportable, ref)
	}

	// Parallel export: fire every export_snapshot back-to-back and let the
	// backend drain them together.
	for _, ref := range exportable {
		uuid, err := m.uploadOne(ref.Name)
		if err != nil {
			m.setUpload(id, cow.RemoteStateFailed, err.Error(), nil)
			return nil, fmt.Errorf("upload %s %s: %w", ref.Role, ref.Name, err)
		}
		switch ref.Role {
		case "rootfs":
			uuids.Rootfs = uuid
		case "memory":
			uuids.Memory = uuid
		case "metadata":
			uuids.Metadata = uuid
		}
	}

	// Serial export (kept for switch-back): wait for each object to leave
	// the drain path before starting the next. The last one does not wait,
	// so Pause still has time for Destroy inside the same RPC budget.
	//
	// settleBy := time.Now().Add(exportSettleBudget)
	// for i, ref := range exportable {
	// 	uuid, err := m.uploadOne(ref.Name)
	// 	if err != nil {
	// 		m.setUpload(id, cow.RemoteStateFailed, err.Error(), nil)
	// 		return nil, fmt.Errorf("upload %s %s: %w", ref.Role, ref.Name, err)
	// 	}
	// 	switch ref.Role {
	// 	case "rootfs":
	// 		uuids.Rootfs = uuid
	// 	case "memory":
	// 		uuids.Memory = uuid
	// 	case "metadata":
	// 		uuids.Metadata = uuid
	// 	}
	// 	if i < len(exportable)-1 {
	// 		m.waitExportSettled(ctx, ref.Name, uuid, settleBy)
	// 	}
	// }
	if uuids.Empty() {
		m.setUpload(id, cow.RemoteStateFailed, "empty remote_uuids", nil)
		return nil, fmt.Errorf("s3 export returned empty remote_uuids for %s", id)
	}
	// Not persisted to catalog.json: by now the package is sealed and its
	// catalog lives on a read-only metadata snapshot, so the write would go
	// to the bare mount point on the host and shadow the real one. cubecow
	// keeps the id on the object, and UploadStatus reads it back from there.
	//
	// Export accepted; upload may still be in flight on the S3 backend.
	m.setUpload(id, cow.RemoteStateRunning, "export started", uuids)
	return uuids, nil
}

// UploadTemplateRootfs exports only the rootfs object of a template package.
//
// A sandbox rootfs is a child snapshot of the template's rootfs, and a
// reference export names just the objects the exported layer owns: the
// child's export carries its own delta and nothing of the parent. Without
// the parent exported, importing that child on another node yields a hole
// where the base layer should be — the ext4 superblock and root inode live
// in the parent, so the volume does not even mount. Exporting the parent
// here is what makes the child resolvable elsewhere.
//
// A template's memory and metadata are not part of that chain (sandbox
// memory is a fresh single-layer volume; metadata derives from a node-local
// base and exports as a merged copy), so they stay node-local.
func (m *S3Cow) UploadTemplateRootfs(ctx context.Context, snapshotID string) (string, error) {
	id := strings.TrimSpace(snapshotID)
	if id == "" {
		return "", fmt.Errorf("snapshot_id is required")
	}
	rootfs := ""
	for _, ref := range activateObjectRefs(ctx, cow.BackendS3, id) {
		if ref.Role == "rootfs" {
			rootfs = strings.TrimSpace(ref.Name)
			break
		}
	}
	if rootfs == "" {
		return "", fmt.Errorf("no rootfs object for %s", id)
	}
	uuid, err := m.uploadOne(rootfs)
	if err != nil {
		return "", fmt.Errorf("upload rootfs %s: %w", rootfs, err)
	}
	// Settle before returning: the template is only usable cross-node once
	// its objects are committed, and nothing later in template creation
	// waits on this.
	m.waitExportSettled(ctx, rootfs, uuid, time.Now().Add(exportSettleBudget))
	return uuid, nil
}

// exportSettleBudget caps the total time Upload spends spacing out a
// package's exports. Pause runs Upload and then a Destroy inside one 120s
// budget, so this leaves room for the Destroy even in the worst case. An
// export pins existing S3 objects rather than copying bytes, so the wait
// is a drain, not a transfer, and is normally seconds.
const exportSettleBudget = 75 * time.Second

// waitExportSettled blocks until this object's export is out of the
// backend's drain path, so the next export does not collide with it.
//
// s3lvol drains one lvstore at a time and gives a waiting export 5s to win
// that race. Every package exports three objects out of the same lvstore,
// and export_snapshot returns its uuid before any bytes move, so issuing
// them back to back — which is what a plain sequential loop does — loses
// two of the three to "Device or resource busy".
//
// Advisory: a wait that runs out of budget or lands on a dead export is
// reported by UploadStatus, not here, because Upload only promises that
// the exports were accepted.
func (m *S3Cow) waitExportSettled(ctx context.Context, name, uuid string, deadline time.Time) {
	for {
		info, infoErr := m.GetVolumeInfo(ctx, name)
		status := ""
		if info != nil {
			status = strings.ToUpper(strings.TrimSpace(info.ExportStatus))
		}
		switch {
		case infoErr == nil && status == cow.ExportStatusDone:
			return
		case infoErr == nil && status == "" && uuid != "":
			// s3lvol dropped the uuid: the export failed and no amount of
			// waiting brings it back.
			CubeLog.Warnf("s3 export %s (%s) died before draining", name, uuid)
			return
		}
		if time.Now().After(deadline) {
			CubeLog.Warnf("s3 export %s (%s) still %q when the settle budget ran out; continuing",
				name, uuid, status)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (m *S3Cow) uploadOne(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("object name is required")
	}
	if IsS3MetadataBaseName(name) {
		return "", fmt.Errorf("refusing to export node-local s3 metadata base %s", name)
	}
	exp, ok := m.engine.(cowSnapshotExporter)
	if !ok || exp == nil {
		return "", fmt.Errorf("cubecow engine does not support export_snapshot")
	}
	// Export is snapshot-only; volumes must be sealed to a snapshot before Upload.
	uuid, err := exp.ExportSnapshot(name)
	if err != nil {
		return "", err
	}
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return "", fmt.Errorf("cubecow_export_snapshot returned empty uuid for %s", name)
	}
	return uuid, nil
}

// UploadStatus implements [cow.Uploader].
// Aggregates cubecow_get_volume_info export_status:
// NONE → pending, empty/INPROGRESS → running, DONE → ready.
func (m *S3Cow) UploadStatus(ctx context.Context, snapshotID string) (*cow.RemoteStatus, error) {
	id := strings.TrimSpace(snapshotID)
	if id == "" {
		return nil, fmt.Errorf("snapshot_id is required")
	}
	st := &cow.RemoteStatus{
		SnapshotID: id,
		State:      cow.RemoteStatePending,
	}

	// No in-memory entry (a restart drops them) is not a gap: the per-object
	// loop below reads each export id straight from cubecow.
	m.uploadLock.Lock()
	entry, ok := m.uploadStates[id]
	m.uploadLock.Unlock()
	if ok {
		st.RemoteUUIDs = entry.uuids
		if entry.state == cow.RemoteStateFailed {
			st.State = cow.RemoteStateFailed
			st.Message = entry.message
			return st, nil
		}
	}

	refs := activateObjectRefs(ctx, cow.BackendS3, id)
	if len(refs) == 0 {
		st.State = cow.RemoteStateRunning
		st.Message = "waiting for local snapshot objects"
		return st, nil
	}

	var (
		sawAny       bool
		allDone      = true
		anyProgress  bool
		anyNone      bool
		anyFailed    bool
		localVolumes []cow.VolumeRemoteInfo
		messages     []string
	)
	for _, ref := range refs {
		if IsS3MetadataBaseName(ref.Name) {
			continue
		}
		info, infoErr := m.GetVolumeInfo(ctx, ref.Name)
		exists, existsErr := cowObjectPresent(info, infoErr)
		if existsErr != nil {
			// Lookup/RPC errors are not an s3lvol export failure. Bubble
			// them so Status leaves State unset and Master stays inprogress.
			return nil, existsErr
		}
		vol := cow.VolumeRemoteInfo{Name: ref.Name, Role: ref.Role, Exists: exists}
		if info != nil {
			vol.DevicePath = strings.TrimSpace(info.DevicePath)
			if ref.Role == "rootfs" && info.Deletable != nil {
				v := *info.Deletable
				st.RootfsDeletable = &v
			}
		}
		localVolumes = append(localVolumes, vol)
		if !exists {
			if ref.Role == "metadata" {
				continue
			}
			allDone = false
			anyProgress = true
			continue
		}
		sawAny = true
		status := ""
		uuid := ""
		if info != nil {
			status = strings.ToUpper(strings.TrimSpace(info.ExportStatus))
			uuid = strings.TrimSpace(info.ExportUUID)
		}
		if uuid != "" {
			if st.RemoteUUIDs == nil {
				st.RemoteUUIDs = &cow.RemoteUUIDs{}
			}
			switch ref.Role {
			case "rootfs":
				if st.RemoteUUIDs.Rootfs == "" {
					st.RemoteUUIDs.Rootfs = uuid
				}
			case "memory":
				if st.RemoteUUIDs.Memory == "" {
					st.RemoteUUIDs.Memory = uuid
				}
			case "metadata":
				if st.RemoteUUIDs.Metadata == "" {
					st.RemoteUUIDs.Metadata = uuid
				}
			}
		}
		switch {
		case status == cow.ExportStatusDone:
			// ok
		case cow.ExportStatusIsFailed(status):
			allDone = false
			anyFailed = true
			messages = append(messages, fmt.Sprintf("%s=%s", ref.Name, status))
		case status == cow.ExportStatusInProgress, status == "":
			// Empty status (with or without a uuid) is not a confirmed
			// s3lvol failure. Keep polling; Master must not stamp failed.
			allDone = false
			anyProgress = true
		case status == cow.ExportStatusNone:
			allDone = false
			anyNone = true
		default:
			allDone = false
			anyProgress = true
			messages = append(messages, fmt.Sprintf("%s=%s", ref.Name, status))
		}
	}
	st.LocalVolumes = localVolumes

	if !sawAny {
		st.State = cow.RemoteStateRunning
		st.Message = "waiting for snapshot volumes"
		return st, nil
	}

	switch {
	case anyFailed:
		st.State = cow.RemoteStateFailed
		st.Message = strings.Join(messages, "; ")
	case allDone:
		st.State = cow.RemoteStateReady
		st.Message = "export_status DONE"
	case anyProgress:
		st.State = cow.RemoteStateRunning
		if len(messages) > 0 {
			st.Message = strings.Join(messages, "; ")
		} else {
			st.Message = "export_status INPROGRESS"
		}
	case anyNone:
		st.State = cow.RemoteStatePending
		st.Message = "export_status NONE"
	default:
		st.State = cow.RemoteStateRunning
		st.Message = "export in progress"
	}
	return st, nil
}

func (m *S3Cow) setUpload(id, state, message string, uuids *cow.RemoteUUIDs) {
	m.uploadLock.Lock()
	defer m.uploadLock.Unlock()
	if m.uploadStates == nil {
		m.uploadStates = make(map[string]s3UploadEntry)
	}
	m.uploadStates[id] = s3UploadEntry{state: state, message: message, uuids: uuids}
}

var _ cow.Uploader = (*S3Cow)(nil)

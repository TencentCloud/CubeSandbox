// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	jsoniter "github.com/json-iterator/go"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
	bolterrors "go.etcd.io/bbolt/errors"
)

// PrepareRuntimeVolumeBackend prepares one existing volume without publishing
// it. The caller must hold the current sandbox's lifecycle lock.
func PrepareRuntimeVolumeBackend(ctx context.Context, req volume.AttachRequest) (*PluginVolumeBackendInfo, error) {
	if localStorage == nil || localStorage.db == nil {
		return nil, errors.New("local storage is unavailable")
	}
	return localStorage.prepareRuntimeVolumeBackend(ctx, req)
}

func (l *local) prepareRuntimeVolumeBackend(ctx context.Context, req volume.AttachRequest) (*PluginVolumeBackendInfo, error) {
	id := req.SandboxID
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsRune(id, 0) ||
		req.Namespace == "" || req.VolumeID == "" || req.Driver == "" {
		return nil, errors.New("runtime volume backend requires sandbox, namespace, volume and driver")
	}
	fallback := filepath.Join(l.failoverDir(), id)
	// Recovery scans prioritize the fallback, while point reads prioritize Bolt.
	// Leave any earlier attempt's file intact until that attempt is resolved.
	if _, err := os.Lstat(fallback); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return nil, fmt.Errorf("check storage fallback: %w", err)
		}
		return nil, errors.New("runtime volume backend requires storage without a pending fallback")
	}
	oldData, err := l.db.Get(bucketName, id)
	if err != nil {
		return nil, fmt.Errorf("read current Bolt storage record: %w", err)
	}
	info := &StorageInfo{}
	if err := jsoniter.ConfigFastest.Unmarshal(oldData, info); err != nil {
		return nil, fmt.Errorf("decode current storage record: %w", err)
	}
	if !matchesRuntimeStorageIdentity(info, req) {
		return nil, errors.New("current storage record does not match sandbox and namespace")
	}
	_, volumeExists := info.Volumes[req.VolumeID]
	_, hostDirExists := info.HostDirBackendInfos[req.VolumeID]
	_, pluginExists := info.PluginVolumeBackendInfos[req.VolumeID]
	if volumeExists || hostDirExists || pluginExists {
		return nil, fmt.Errorf("sandbox storage already contains volume %q", req.VolumeID)
	}

	mgr := volume.Global()
	holders := mgr.RefCountStore()
	if holders == nil {
		return nil, errors.New("plugin volume reference store unavailable")
	}
	record, _, err := holders.GetRecord(req.Namespace, req.VolumeID)
	if err != nil {
		return nil, fmt.Errorf("check current volume holder: %w", err)
	}
	if _, held := record.SandboxIDs[id]; held {
		return nil, fmt.Errorf("sandbox already holds plugin volume %q", req.VolumeID)
	}
	req.VolumeBaseDir = l.volumePluginBaseDir()
	if err := os.MkdirAll(req.VolumeBaseDir, 0755); err != nil {
		return nil, fmt.Errorf("create volume plugin base directory: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, err := mgr.Attach(ctx, &req)
	if err != nil {
		return nil, fmt.Errorf("attach plugin volume %q: %w", req.VolumeID, err)
	}
	if result == nil {
		return nil, fmt.Errorf("plugin volume %q returned no backend or cleanup metadata", req.VolumeID)
	}

	backend := &PluginVolumeBackendInfo{
		VolumeID: req.VolumeID,
		Driver:   req.Driver,
		HostPath: result.HostPath,
		Metadata: maps.Clone(result.Metadata),
	}
	if info.PluginVolumeBackendInfos == nil {
		info.PluginVolumeBackendInfos = make(map[string]*PluginVolumeBackendInfo)
	}
	info.PluginVolumeBackendInfos[req.VolumeID] = backend

	// Keep the cleanup inputs even when the host path cannot be published.
	if err := validateHostPathUnderBase(backend.HostPath, req.VolumeBaseDir); err != nil {
		cleanupErr := detachRuntimeVolumeBackend(ctx, mgr, req, backend)
		if cleanupErr == nil {
			return nil, err
		}
		return nil, errors.Join(err, cleanupErr, l.writeBackendFileInfo(ctx, id, info))
	}

	boltErr, writeErr := l.writeBackendFileInfoWithFallback(id, info)
	if writeErr != nil {
		if boltErr == bolterrors.ErrDatabaseReadOnly {
			_, fallbackErr := os.Lstat(fallback)
			if errors.Is(fallbackErr, os.ErrNotExist) {
				return nil, errors.Join(writeErr, detachRuntimeVolumeBackend(ctx, mgr, req, backend))
			}
		}
		return nil, writeErr
	}
	stored, err := l.readBackendFileInfo(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read back plugin volume %q: %w", req.VolumeID, err)
	}
	if matchesRuntimeStorageIdentity(stored, req) && matchesRuntimeVolumeBackend(stored.PluginVolumeBackendInfos[req.VolumeID], backend) {
		if boltErr != nil {
			return nil, fmt.Errorf("verify committed Bolt storage for plugin volume %q: %w", req.VolumeID, boltErr)
		}
		return stored.PluginVolumeBackendInfos[req.VolumeID], nil
	}

	readbackErr := fmt.Errorf("storage read-back did not confirm plugin volume %q", req.VolumeID)
	if boltErr != bolterrors.ErrDatabaseReadOnly || !matchesRuntimeStorageIdentity(stored, req) {
		return nil, errors.Join(readbackErr, boltErr)
	}
	// Only remove our fallback once Bolt still holds the previous record and the
	// backend has been detached. Other write outcomes can be ambiguous.
	currentData, err := l.db.Get(bucketName, id)
	if err != nil || !bytes.Equal(currentData, oldData) {
		return nil, errors.Join(readbackErr, err)
	}
	fallbackData, err := os.ReadFile(fallback)
	if err != nil {
		return nil, errors.Join(readbackErr, err)
	}
	fallbackInfo := &StorageInfo{}
	if err := jsoniter.ConfigFastest.Unmarshal(fallbackData, fallbackInfo); err != nil {
		return nil, errors.Join(readbackErr, err)
	}
	if !matchesRuntimeStorageIdentity(fallbackInfo, req) || !matchesRuntimeVolumeBackend(fallbackInfo.PluginVolumeBackendInfos[req.VolumeID], backend) {
		return nil, readbackErr
	}
	if err := detachRuntimeVolumeBackend(ctx, mgr, req, backend); err != nil {
		return nil, errors.Join(readbackErr, err)
	}
	return nil, errors.Join(readbackErr, os.Remove(fallback))
}

func matchesRuntimeStorageIdentity(info *StorageInfo, req volume.AttachRequest) bool {
	return info.SandboxID == req.SandboxID && info.Namespace == req.Namespace
}

func matchesRuntimeVolumeBackend(got, want *PluginVolumeBackendInfo) bool {
	return got != nil && got.VolumeID == want.VolumeID && got.Driver == want.Driver &&
		got.HostPath == want.HostPath && got.BindPath == want.BindPath && maps.Equal(got.Metadata, want.Metadata)
}

func detachRuntimeVolumeBackend(ctx context.Context, mgr *volume.Manager, req volume.AttachRequest, backend *PluginVolumeBackendInfo) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := mgr.Detach(cleanupCtx, &volume.DetachRequest{
		SandboxID:             req.SandboxID,
		Namespace:             req.Namespace,
		VolumeID:              backend.VolumeID,
		Driver:                backend.Driver,
		Metadata:              maps.Clone(backend.Metadata),
		RequireRecordedHolder: true,
	}); err != nil {
		return fmt.Errorf("clean up plugin volume %q: %w", backend.VolumeID, err)
	}
	return nil
}

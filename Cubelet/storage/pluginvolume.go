// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	cubebox "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"golang.org/x/sys/unix"
)

// PluginVolumeBackendInfo is persisted inside StorageInfo.PluginVolumeBackendInfos
// for every volume that was attached via the VolumePlugin framework.
// It carries exactly what is needed to call VolumePlugin.Detach later.
type PluginVolumeBackendInfo struct {
	// VolumeID is the CubeMaster VolumeRecord identifier.
	VolumeID string `json:"volume_id"`
	// Driver is the PluginVolumeSource.driver value used at Attach time.
	Driver string `json:"driver"`
	// HostPath is the host-side path returned by VolumePlugin.Attach.
	HostPath string `json:"host_path"`
	// Metadata is the opaque map returned by VolumePlugin.Attach and passed
	// verbatim back to VolumePlugin.Detach.
	Metadata map[string]string `json:"metadata,omitempty"`
	// BindPath is the per-sandbox bind-mount target created during Attach
	// (e.g. /data/cubelet/hostdir/<sandboxID>/rw/<volName>).
	// Stored so that Detach can unmount it without reconstructing the path.
	BindPath string `json:"bind_path,omitempty"`
}

// attachPluginVolume is called from local.Create for volumes whose VolumeSource
// has a non-nil plugin_volume field, or whose name appears in the
// "plugin-volume-sources" annotation.
//
// On success, a PluginVolumeBackendInfo entry is appended to result so that
// Destroy / CleanUp can later reconstruct the Detach call.
//
// The ref-count for (namespace, volumeID) is incremented via Manager.Attach;
// the pre-attach count is embedded in AttachRequest.RefCount so the plugin
// can decide whether host-level setup is needed (RefCount == 0 → first attach).
// pluginVolumeSourceEntry is one element of the plugin-volume-sources annotation.
type pluginVolumeSourceEntry struct {
	Name        string `json:"name"`
	Driver      string `json:"driver"`
	PrivateData string `json:"private_data"`
}

// lookupPluginVolumeSource returns driver and private_data for volumeName from
// the CubeMaster-injected plugin-volume-sources annotation. ok is false when
// the annotation is missing, malformed, or does not list volumeName.
func lookupPluginVolumeSource(annotations map[string]string, volumeName string) (driver, privateData string, ok bool) {
	if annotations == nil {
		return "", "", false
	}
	raw := annotations["plugin-volume-sources"]
	if raw == "" {
		return "", "", false
	}
	var entries []pluginVolumeSourceEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return "", "", false
	}
	for _, e := range entries {
		if e.Name == volumeName {
			return e.Driver, e.PrivateData, true
		}
	}
	return "", "", false
}

// isPluginVolume reports whether volumeName is listed in the
// "plugin-volume-sources" annotation injected by CubeMaster.
func isPluginVolume(annotations map[string]string, volumeName string) bool {
	_, _, ok := lookupPluginVolumeSource(annotations, volumeName)
	return ok
}

// volumePluginBaseDir returns the configured parent directory that every
// plugin_volume Attach must mount under, falling back to the built-in default.
func (l *local) volumePluginBaseDir() string {
	if l.config != nil && l.config.VolumePluginBaseDir != "" {
		return filepath.Clean(l.config.VolumePluginBaseDir)
	}
	return defaultVolumePluginBaseDir
}

// validateHostPathUnderBase enforces the Cubelet requirement that a plugin's
// returned host_path is an absolute path located strictly inside baseDir.
func validateHostPathUnderBase(hostPath, baseDir string) error {
	if hostPath == "" {
		return fmt.Errorf("plugin returned empty host_path")
	}
	if !filepath.IsAbs(hostPath) {
		return fmt.Errorf("host_path %q is not absolute", hostPath)
	}
	rel, err := filepath.Rel(filepath.Clean(baseDir), filepath.Clean(hostPath))
	if err != nil {
		return fmt.Errorf("host_path %q not under required base dir %q: %w", hostPath, baseDir, err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("host_path %q must be a subdirectory of required base dir %q", hostPath, baseDir)
	}
	return nil
}

func (l *local) attachPluginVolume(
	ctx context.Context,
	opts *workflow.CreateContext,
	v *cubebox.Volume,
	result *StorageInfo,
) error {
	// Resolve driver + private_data: first try the plugin-volume-sources
	// annotation (preferred path set by CubeMaster), then fall back to
	// VolumeSource.plugin_volume for driver only.
	volumeName := v.GetName()
	driver, privateData, _ := lookupPluginVolumeSource(opts.ReqInfo.GetAnnotations(), volumeName)

	// Fallback: VolumeSource.plugin_volume (proto field 11).
	if driver == "" {
		pv := v.GetVolumeSource().GetPluginVolume()
		if pv == nil {
			// Not a plugin volume — nothing to do.
			return nil
		}
		driver = pv.GetDriver()
	}

	if driver == "" {
		return fmt.Errorf("plugin_volume %q: driver is empty", volumeName)
	}

	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return fmt.Errorf("plugin_volume %q: namespace: %w", volumeName, err)
	}

	mgr := volume.Global()
	if !mgr.Has(driver) {
		return fmt.Errorf("plugin_volume %q: no plugin registered for driver %q", volumeName, driver)
	}

	// volumeID == volume.Name (set by CubeMaster when creating the volume spec).
	volumeID := volumeName

	// The plugin must mount the volume under this parent directory; ensure it
	// exists before invoking the plugin so the plugin can create its subdir.
	volumeBaseDir := l.volumePluginBaseDir()
	if err := os.MkdirAll(volumeBaseDir, 0755); err != nil {
		return fmt.Errorf("plugin_volume %q: mkdir base dir %s: %w", volumeName, volumeBaseDir, err)
	}

	req := &volume.AttachRequest{
		SandboxID:     opts.SandboxID,
		Namespace:     ns,
		VolumeID:      volumeID,
		Driver:        driver,
		VolumeBaseDir: volumeBaseDir,
		PrivateData:   privateData,
		// RefCount (pre-attach) filled in by Manager.Attach after Acquire.
	}

	priorAttempt := opts.PluginVolumeAttachAttempted
	opts.PluginVolumeAttachAttempted = true
	opts.PluginVolumeAttachUncertain = true
	res, err := mgr.Attach(ctx, req)
	if err != nil {
		var notStarted *volume.AttachNotStartedError
		if errors.As(err, &notStarted) {
			opts.PluginVolumeAttachAttempted = priorAttempt
			opts.PluginVolumeAttachUncertain = false
		}
		return fmt.Errorf("plugin_volume %q (driver=%s): attach: %w", volumeName, driver, err)
	}

	// Record backend info immediately after Attach so create-failure rollback
	// can Detach even if host-path validation or the bind mount fails.
	if result.PluginVolumeBackendInfos == nil {
		result.PluginVolumeBackendInfos = make(map[string]*PluginVolumeBackendInfo)
	}
	result.PluginVolumeBackendInfos[volumeName] = &PluginVolumeBackendInfo{
		VolumeID: volumeID,
		Driver:   driver,
		HostPath: res.HostPath,
		Metadata: res.Metadata,
	}

	opts.PluginVolumeIDs = append(opts.PluginVolumeIDs, volumeID)
	opts.PluginVolumeAttachUncertain = false
	if err := validateHostPathUnderBase(res.HostPath, volumeBaseDir); err != nil {
		return fmt.Errorf("plugin_volume %q (driver=%s): %w", volumeName, driver, err)
	}
	log.G(ctx).Infof("[plugin_volume] attached %q via driver=%s volumeID=%s hostPath=%s refcount_before=%d",
		volumeName, driver, volumeID, res.HostPath, req.RefCount)

	// Bind-mount the plugin's hostPath into the virtiofs share directory so
	// the existing host-dir mount infrastructure carries it into the sandbox VM.
	roStr := "rw"
	readOnly := false
	for _, c := range opts.ReqInfo.GetContainers() {
		for _, vm := range c.GetVolumeMounts() {
			if vm.GetName() == volumeName && vm.GetReadonly() {
				roStr = "ro"
				readOnly = true
			}
		}
	}

	shareDir := filepath.Join(hostDirBasePath, opts.SandboxID, roStr)
	bindDest := filepath.Join(shareDir, volumeName)
	if err := os.MkdirAll(bindDest, 0755); err != nil {
		return fmt.Errorf("plugin_volume %q: mkdir %s: %w", volumeName, bindDest, err)
	}
	flags := uintptr(unix.MS_BIND | unix.MS_REC)
	if err := unix.Mount(res.HostPath, bindDest, "", flags, ""); err != nil {
		return fmt.Errorf("plugin_volume %q: bind mount %s -> %s: %w", volumeName, res.HostPath, bindDest, err)
	}
	// Persist BindPath before remount so create rollback can unmount it if
	// the readonly remount fails.
	result.PluginVolumeBackendInfos[volumeName].BindPath = bindDest
	if readOnly {
		roFlags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY)
		if err := unix.Mount("", bindDest, "", roFlags, ""); err != nil {
			return fmt.Errorf("plugin_volume %q: remount ro %s: %w", volumeName, bindDest, err)
		}
	}
	log.G(ctx).Infof("[plugin_volume] bind-mounted %s -> %s (ro=%v)", res.HostPath, bindDest, readOnly)

	if result.HostDirBackendInfos == nil {
		result.HostDirBackendInfos = make(map[string]*HostDirBackendInfo)
	}
	result.HostDirBackendInfos[volumeName] = &HostDirBackendInfo{
		VolumeName: volumeName,
		ShareDir:   shareDir,
		BindPath:   bindDest,
		ReadOnly:   readOnly,
	}
	return nil
}

// detachPluginVolumes is called from local.destroy (and from Init during
// restart cleanup) for all plugin_volume entries in info.PluginVolumeBackendInfos.
//
// For each volume Manager.Detach releases the local ref-count, invokes the
// plugin, and rolls the count back if the plugin fails. The plugin receives
// the post-detach count in DetachRequest.RefCount so it can decide whether to
// tear down host-level resources (RefCount == 0 → last detach).
//
// Errors are collected and returned together so that one failing plugin does
// not prevent other volumes from being cleaned up.
func (l *local) detachPluginVolumes(
	ctx context.Context,
	info *StorageInfo,
	opts *workflow.DestroyContext,
) error {
	if len(info.PluginVolumeBackendInfos) == 0 {
		return nil
	}

	mgr := volume.Global()
	var errs []error
	var create *workflow.CreateContext
	if opts != nil {
		create = opts.CreateVolumeRollback
	}
	if create != nil {
		if info.SandboxID != create.SandboxID {
			return fmt.Errorf("rollback storage metadata belongs to sandbox %q", info.SandboxID)
		}
		for _, id := range create.PluginVolumeIDs {
			if pbi := info.PluginVolumeBackendInfos[id]; pbi == nil || pbi.VolumeID != id {
				return fmt.Errorf("rollback storage metadata missing plugin volume %q", id)
			}
		}
	}

	for volName, pbi := range info.PluginVolumeBackendInfos {
		req := &volume.DetachRequest{
			SandboxID:             info.SandboxID,
			Namespace:             info.Namespace,
			VolumeID:              pbi.VolumeID,
			Driver:                pbi.Driver,
			Metadata:              pbi.Metadata,
			RequireRecordedHolder: create != nil,
			// RefCount (post-detach) filled in by Manager.Detach after Release.
		}
		if err := mgr.Detach(ctx, req); err != nil {
			log.G(ctx).Errorf("[plugin_volume] detach %q (driver=%s volumeID=%s) failed: %v",
				volName, pbi.Driver, pbi.VolumeID, err)
			errs = append(errs, fmt.Errorf("plugin_volume %q (driver=%s): detach: %w",
				volName, pbi.Driver, err))
			// Manager.Detach rolls back the local ref-count on plugin failure and
			// clears NodeRefLastDetach — do not report 1→0 to CubeMaster.
		} else {
			log.G(ctx).Infof("[plugin_volume] detached %q (driver=%s volumeID=%s) refcount_after=%d",
				volName, pbi.Driver, pbi.VolumeID, req.RefCount)

			// Report a 1→0 node-level transition only after a successful Detach.
			// A waiting Create can report this release even if the +1 came
			// from a different successful holder of the same mount.
			if opts != nil && (!opts.IsRollBack || create != nil) && req.NodeRefLastDetach {
				opts.VolumeRefEvents = append(opts.VolumeRefEvents, workflow.VolumeRefEvent{
					VolumeID:            pbi.VolumeID,
					Referenced:          0,
					CreateReportClaimed: req.CreateReportClaimed,
				})
			}
		}

		// Unmount the per-sandbox bind mount that was created during attach.
		// The host-level FUSE mount (pbi.HostPath) is intentionally left
		// mounted — it is shared across all sandboxes using the same volume.
		if pbi.BindPath != "" {
			if err := unix.Unmount(pbi.BindPath, unix.MNT_DETACH); err != nil {
				log.G(ctx).Warnf("[plugin_volume] unmount bind %q failed (ignored): %v", pbi.BindPath, err)
				if create != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
					errs = append(errs, fmt.Errorf("unmount plugin volume bind %q: %w", pbi.BindPath, err))
				}
			} else {
				log.G(ctx).Infof("[plugin_volume] unmounted bind %q", pbi.BindPath)
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("detachPluginVolumes: %v", errs)
	}
	if create != nil {
		create.PluginVolumeRollback = append([]workflow.VolumeRefEvent{}, opts.VolumeRefEvents...)
	}
	return nil
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
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
// isPluginVolume reports whether volumeName is listed in the
// "plugin-volume-sources" annotation injected by CubeMaster.
func isPluginVolume(annotations map[string]string, volumeName string) bool {
	if annotations == nil {
		return false
	}
	raw := annotations["plugin-volume-sources"]
	if raw == "" {
		return false
	}
	type entry struct {
		Name string `json:"name"`
	}
	var entries []entry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name == volumeName {
			return true
		}
	}
	return false
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
	// Resolve driver: first try the plugin-volume-sources annotation (preferred
	// path set by CubeMaster), then fall back to VolumeSource.plugin_volume.
	driver := ""
	volumeName := v.GetName()

	if annotations := opts.ReqInfo.GetAnnotations(); annotations != nil {
		if raw := annotations["plugin-volume-sources"]; raw != "" {
			type entry struct {
				Name   string `json:"name"`
				Driver string `json:"driver"`
			}
			var entries []entry
			if err := json.Unmarshal([]byte(raw), &entries); err == nil {
				for _, e := range entries {
					if e.Name == volumeName {
						driver = e.Driver
						break
					}
				}
			}
		}
	}

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
		// RefCount (pre-attach) filled in by Manager.Attach after Acquire.
	}

	res, err := mgr.Attach(ctx, req)
	if err != nil {
		return fmt.Errorf("plugin_volume %q (driver=%s): attach: %w", volumeName, driver, err)
	}

	// Enforce that the plugin honoured the required base directory: the
	// returned hostPath must resolve to a path inside volumeBaseDir.
	if err := validateHostPathUnderBase(res.HostPath, volumeBaseDir); err != nil {
		// Roll back the attach we just performed so the ref-count and any host
		// mount the plugin created are released.
		detachReq := &volume.DetachRequest{
			SandboxID: opts.SandboxID,
			Namespace: ns,
			VolumeID:  volumeID,
			Driver:    driver,
			Metadata:  res.Metadata,
		}
		if derr := mgr.Detach(ctx, detachReq); derr != nil {
			log.G(ctx).Warnf("[plugin_volume] rollback detach after invalid hostPath failed: %v", derr)
		}
		return fmt.Errorf("plugin_volume %q (driver=%s): %w", volumeName, driver, err)
	}

	log.G(ctx).Infof("[plugin_volume] attached %q via driver=%s volumeID=%s hostPath=%s refcount_before=%d",
		volumeName, driver, volumeID, res.HostPath, req.RefCount)

	// Record backend info immediately after Attach so create-failure rollback
	// (cleanupCreateResult → destroy → detachPluginVolumes) can Detach even if
	// mkdir/bind/remount below fails.
	if result.PluginVolumeBackendInfos == nil {
		result.PluginVolumeBackendInfos = make(map[string]*PluginVolumeBackendInfo)
	}
	result.PluginVolumeBackendInfos[volumeName] = &PluginVolumeBackendInfo{
		VolumeID: volumeID,
		Driver:   driver,
		HostPath: res.HostPath,
		Metadata: res.Metadata,
	}

	// Report a 0→1 node-level transition to CubeMaster (via the create
	// response ext_info) so it can increment its cross-node ref-count.
	// Repeat references on this node (RefCount>0) emit nothing.
	// Events are only published when create succeeds (see setCubeExtKey).
	if req.NodeRefFirstAttach && opts != nil {
		opts.VolumeRefEvents = append(opts.VolumeRefEvents, workflow.VolumeRefEvent{
			VolumeID:   volumeID,
			Referenced: 1,
		})
	}

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

	for volName, pbi := range info.PluginVolumeBackendInfos {
		req := &volume.DetachRequest{
			SandboxID: info.SandboxID,
			Namespace: info.Namespace,
			VolumeID:  pbi.VolumeID,
			Driver:    pbi.Driver,
			Metadata:  pbi.Metadata,
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
			// Skip rollback destroys (create failure cleanup): those must not
			// emit refcount events to CubeMaster.
			if opts != nil && !opts.IsRollBack && req.NodeRefLastDetach {
				opts.VolumeRefEvents = append(opts.VolumeRefEvents, workflow.VolumeRefEvent{
					VolumeID:   pbi.VolumeID,
					Referenced: 0,
				})
			}
		}

		// Unmount the per-sandbox bind mount that was created during attach.
		// The host-level FUSE mount (pbi.HostPath) is intentionally left
		// mounted — it is shared across all sandboxes using the same volume.
		if pbi.BindPath != "" {
			if err := unix.Unmount(pbi.BindPath, unix.MNT_DETACH); err != nil {
				log.G(ctx).Warnf("[plugin_volume] unmount bind %q failed (ignored): %v", pbi.BindPath, err)
			} else {
				log.G(ctx).Infof("[plugin_volume] unmounted bind %q", pbi.BindPath)
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("detachPluginVolumes: %v", errs)
	}
	return nil
}

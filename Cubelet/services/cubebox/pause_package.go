// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"google.golang.org/protobuf/proto"
)

// pauseSandboxSpecFileName is packed next to catalog / CoW snapshot files so
// cross-node Resume can recreate without Master holding the full sandboxspec.
const pauseSandboxSpecFileName = "sandbox_spec.json"

// buildPauseSandboxSpec captures Cubelet-level recreate payload from a live CubeBox.
// Includes the same recreate-needed network fields Master keeps in sandboxspec
// (cube_network_config / network_type / exposed_ports / runtime_handler) so
// Resume Create rebuilds the NIC with the original egress policy.
func buildPauseSandboxSpec(sb *cubeboxstore.CubeBox, requestID string) *cubebox.RunCubeSandboxRequest {
	runReq := &cubebox.RunCubeSandboxRequest{
		RequestID:         requestID,
		InstanceType:      sb.InstanceType,
		Namespace:         sb.Namespace,
		Volumes:           sb.Volumes,
		Annotations:       cloneStringMap(sb.Annotations),
		Labels:            cloneStringMap(sb.Labels),
		NetworkType:       sb.NetworkType,
		RuntimeHandler:    sb.RuntimeHandler,
		ExposedPorts:      append([]int64(nil), sb.ExposedPorts...),
		CubeNetworkConfig: cloneCubeNetworkConfig(sb.CubeNetworkConfig),
	}
	if runReq.Annotations == nil {
		runReq.Annotations = map[string]string{}
	}
	if runReq.InstanceType == "" {
		runReq.InstanceType = cubebox.InstanceType_cubebox.String()
	}
	for _, c := range sb.AllContainers() {
		if c == nil || c.Config == nil {
			continue
		}
		runReq.Containers = append(runReq.Containers, containerConfigForPause(c))
	}
	return runReq
}

func cloneCubeNetworkConfig(in *cubebox.CubeNetworkConfig) *cubebox.CubeNetworkConfig {
	if in == nil {
		return nil
	}
	cloned, _ := proto.Clone(in).(*cubebox.CubeNetworkConfig)
	return cloned
}

// containerConfigForPause clones stored Config and fills args/envs/cwd from the
// live OCI spec when makeContainerConfigToSave had stripped them (older Cubelets).
func containerConfigForPause(c *cubeboxstore.Container) *cubebox.ContainerConfig {
	cloned, _ := proto.Clone(c.Config).(*cubebox.ContainerConfig)
	if cloned == nil {
		return c.Config
	}
	if c.Container == nil {
		return cloned
	}
	spec, err := c.Container.Spec(context.Background())
	if err != nil || spec == nil || spec.Process == nil {
		return cloned
	}
	if len(cloned.GetCommand()) == 0 && len(cloned.GetArgs()) == 0 && len(spec.Process.Args) > 0 {
		cloned.Args = append([]string{}, spec.Process.Args...)
	}
	if cloned.GetWorkingDir() == "" && spec.Process.Cwd != "" {
		cloned.WorkingDir = spec.Process.Cwd
	}
	if len(cloned.GetEnvs()) == 0 && len(spec.Process.Env) > 0 {
		for _, e := range spec.Process.Env {
			k, v, ok := strings.Cut(e, "=")
			if !ok {
				continue
			}
			cloned.Envs = append(cloned.Envs, &cubebox.KeyValue{Key: k, Value: v})
		}
	}
	if cloned.GetSecurityContext() == nil {
		cloned.SecurityContext = &cubebox.ContainerSecurityContext{Privileged: true}
	}
	return cloned
}

func pauseSpecDir(entry *storage.SnapshotCatalogEntry) string {
	if entry == nil {
		return ""
	}
	if d := strings.TrimSpace(entry.MetaDir); d != "" {
		return d
	}
	return strings.TrimSpace(entry.SnapshotPath)
}

// pauseSpecDirFor is where this Resume reads sandbox_spec.json from, with the
// disk holding it mounted.
//
// Same-node that is the pause package, found through the catalog and mounted
// on demand because Finalize seals and unmounts it after Pause. Cross-node
// there is no package to look up: the Create entry already imported a copy of
// the very same disk under the sandbox's own name, so the spec is read from
// there and the catalog is never consulted.
func pauseSpecDirFor(ctx context.Context, req *cubebox.RunCubeSandboxRequest, backend, snapID string) (string, error) {
	if imp := storage.CrossNodeSandboxImport(req.GetAnnotations()); imp != nil {
		return imp.EnsureMetadata(ctx)
	}
	entry, err := storage.GetLocalSnapshotFor(ctx, backend, snapID)
	if err != nil {
		return "", fmt.Errorf("load pause snapshot catalog %s: %w", snapID, err)
	}
	if !strings.EqualFold(strings.TrimSpace(entry.Kind), storage.CatalogKindPauseSnapshot) {
		return "", fmt.Errorf("snapshot %s kind=%q is not pause_snapshot", snapID, entry.Kind)
	}
	specDir := pauseSpecDir(entry)
	if err := storage.MountS3MetadataAt(ctx, backend, snapID, specDir); err != nil {
		return "", fmt.Errorf("mount pause metadata for sandbox_spec: %w", err)
	}
	return specDir, nil
}

func writePauseSandboxSpec(snapshotPath string, runReq *cubebox.RunCubeSandboxRequest) error {
	if runReq == nil {
		return fmt.Errorf("nil sandbox_spec")
	}
	body, err := json.MarshalIndent(runReq, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(snapshotPath, pauseSandboxSpecFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadPauseSandboxSpec(snapshotPath string) (*cubebox.RunCubeSandboxRequest, error) {
	runReq, err := storage.LoadPauseSandboxSpec(snapshotPath)
	if err != nil {
		return nil, err
	}
	if len(runReq.Containers) == 0 {
		return nil, fmt.Errorf("sandbox_spec missing containers")
	}
	return runReq, nil
}

// pauseRestoreBinding is the part of a Resume that only the Master-sent thin
// Create knows. The packed sandbox_spec describes the sandbox as it ran, so
// it says nothing about which package this Resume restores from, nor where
// that package's bytes come from — and expanding the request overwrites
// everything the thin Create carried.
type pauseRestoreBinding struct {
	snapshotID       string
	backend          string
	requestID        string
	desiredSandboxID string
	attachedAt       string
	remoteUUIDs      string
	crossNode        bool
}

func pauseRestoreBindingFrom(req *cubebox.RunCubeSandboxRequest, snapshotID, backend string) pauseRestoreBinding {
	ann := req.GetAnnotations()
	return pauseRestoreBinding{
		snapshotID:       snapshotID,
		backend:          backend,
		requestID:        req.GetRequestID(),
		desiredSandboxID: strings.TrimSpace(ann[constants.MasterAnnotationDesiredSandboxID]),
		attachedAt:       strings.TrimSpace(ann[constants.MasterAnnotationRuntimeSnapshotAttachedAt]),
		remoteUUIDs:      strings.TrimSpace(ann[constants.MasterAnnotationSnapshotRemoteUUIDs]),
		crossNode:        isCrossNodeRestore(ann),
	}
}

// applyTo stamps the binding back onto the request the packed spec replaced.
// Losing remoteUUIDs here sends the rootfs path down the clone branch instead
// of using the volume imported from S3, and on a node that never held the
// package there is no snapshot to clone from.
func (b pauseRestoreBinding) applyTo(req *cubebox.RunCubeSandboxRequest) {
	if req.Annotations == nil {
		req.Annotations = map[string]string{}
	}
	if b.requestID != "" {
		req.RequestID = b.requestID
	}
	if b.desiredSandboxID != "" {
		req.Annotations[constants.MasterAnnotationDesiredSandboxID] = b.desiredSandboxID
	}
	req.Annotations[constants.MasterAnnotationRuntimeSnapshotID] = b.snapshotID
	req.Annotations[constants.MasterAnnotationPauseSnapshotID] = b.snapshotID
	attachedAt := b.attachedAt
	if attachedAt == "" {
		attachedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	req.Annotations[constants.MasterAnnotationRuntimeSnapshotAttachedAt] = attachedAt
	req.Annotations[constants.MasterAnnotationStorageBackend] = b.backend
	if strings.TrimSpace(req.GetBackend()) == "" {
		req.Backend = b.backend
	}
	if b.remoteUUIDs != "" {
		req.Annotations[constants.MasterAnnotationSnapshotRemoteUUIDs] = b.remoteUUIDs
	}
	if b.crossNode {
		req.Annotations[constants.MasterAnnotationSnapshotCrossNode] = "true"
	}
	delete(req.Annotations, constants.MasterAnnotationsAppSnapshotCreate)
	// Keep appsnapshot.template.id=tpl-* so Resume can EnsureCubeRunTemplate
	// the original template (kernel／image). GetSnapshotTemplateID still
	// prefers runtime.snapshot.id=snap-* while pause.snapshot.id is set, so
	// storage restore stays on the pause catalog.
}

// expandPauseSnapshotPackage fills a Master-thin Create request from the
// sandbox_spec.json packed inside the pause snapshot directory.
func expandPauseSnapshotPackage(req *cubebox.RunCubeSandboxRequest) error {
	if req == nil {
		return fmt.Errorf("nil create request")
	}
	ann := req.GetAnnotations()
	if ann == nil {
		return nil
	}
	snapID := strings.TrimSpace(ann[constants.MasterAnnotationPauseSnapshotID])
	if snapID == "" {
		return nil
	}
	if err := pathutil.ValidateSafeID(snapID); err != nil {
		return fmt.Errorf("invalid pause snapshot id: %w", err)
	}
	backend, berr := storageBackendFromAnnotations(req.GetAnnotations())
	if berr != nil {
		return fmt.Errorf("pause snapshot backend: %w", berr)
	}
	ctx := context.Background()
	specDir, err := pauseSpecDirFor(ctx, req, backend, snapID)
	if err != nil {
		return err
	}
	packed, err := loadPauseSandboxSpec(specDir)
	if err != nil {
		return fmt.Errorf("load pause sandbox_spec: %w", err)
	}

	binding := pauseRestoreBindingFrom(req, snapID, backend)
	// Thin Create may carry recreate-needed network fields from Master
	// sandboxspec (legacy pause packages omit them in sandbox_spec.json).
	thinNetwork := req.GetCubeNetworkConfig()
	thinNetworkType := req.GetNetworkType()
	thinRuntimeHandler := req.GetRuntimeHandler()
	thinExposedPorts := append([]int64(nil), req.GetExposedPorts()...)

	*req = *packed
	binding.applyTo(req)

	// Prefer packed values; fall back to thin Create for older pause snaps.
	if req.GetCubeNetworkConfig() == nil {
		req.CubeNetworkConfig = thinNetwork
	}
	if strings.TrimSpace(req.GetNetworkType()) == "" {
		req.NetworkType = thinNetworkType
	}
	if strings.TrimSpace(req.GetRuntimeHandler()) == "" {
		req.RuntimeHandler = thinRuntimeHandler
	}
	if len(req.GetExposedPorts()) == 0 {
		req.ExposedPorts = thinExposedPorts
	}
	return nil
}

// resumeFromPauseSandboxID returns the sandboxID for Create-from-pause so the
// caller can take sandboxLifecycleLocks with Pause/Destroy. Empty means a
// normal Create (no pause snapshot annotation).
func resumeFromPauseSandboxID(req *cubebox.RunCubeSandboxRequest) string {
	if req == nil {
		return ""
	}
	ann := req.GetAnnotations()
	if ann == nil {
		return ""
	}
	if strings.TrimSpace(ann[constants.MasterAnnotationPauseSnapshotID]) == "" {
		return ""
	}
	return strings.TrimSpace(ann[constants.MasterAnnotationDesiredSandboxID])
}

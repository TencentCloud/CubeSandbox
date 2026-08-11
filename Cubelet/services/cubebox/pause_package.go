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

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
	"google.golang.org/protobuf/proto"
)

// pauseSandboxSpecFileName is packed next to catalog / CoW snapshot files so
// cross-node Resume can recreate without Master holding the full sandboxspec.
const pauseSandboxSpecFileName = "sandbox_spec.json"

// buildPauseSandboxSpec captures Cubelet-level recreate payload from a live CubeBox.
func buildPauseSandboxSpec(sb *cubeboxstore.CubeBox, requestID string) *cubebox.RunCubeSandboxRequest {
	runReq := &cubebox.RunCubeSandboxRequest{
		RequestID:    requestID,
		InstanceType: sb.InstanceType,
		Namespace:    sb.Namespace,
		Volumes:      sb.Volumes,
		Annotations:  cloneStringMap(sb.Annotations),
		Labels:       cloneStringMap(sb.Labels),
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

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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
	entry, err := storage.GetLocalSnapshot(context.Background(), snapID)
	if err != nil {
		return fmt.Errorf("load pause snapshot catalog %s: %w", snapID, err)
	}
	if !strings.EqualFold(strings.TrimSpace(entry.Kind), storage.CatalogKindPauseSnapshot) {
		return fmt.Errorf("snapshot %s kind=%q is not pause_snapshot", snapID, entry.Kind)
	}
	packed, err := loadPauseSandboxSpec(entry.SnapshotPath)
	if err != nil {
		return fmt.Errorf("load pause sandbox_spec: %w", err)
	}

	// Preserve Master-owned identity / snapshot binding from the thin request.
	desiredID := strings.TrimSpace(ann[constants.MasterAnnotationDesiredSandboxID])
	attachedAt := strings.TrimSpace(ann[constants.MasterAnnotationRuntimeSnapshotAttachedAt])
	requestID := req.GetRequestID()

	*req = *packed
	if req.Annotations == nil {
		req.Annotations = map[string]string{}
	}
	if requestID != "" {
		req.RequestID = requestID
	}
	if desiredID != "" {
		req.Annotations[constants.MasterAnnotationDesiredSandboxID] = desiredID
	}
	req.Annotations[constants.MasterAnnotationRuntimeSnapshotID] = snapID
	req.Annotations[constants.MasterAnnotationPauseSnapshotID] = snapID
	if attachedAt == "" {
		attachedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	req.Annotations[constants.MasterAnnotationRuntimeSnapshotAttachedAt] = attachedAt
	delete(req.Annotations, constants.MasterAnnotationsAppSnapshotCreate)
	// Packed Create-from-template still carries appsnapshot.template.id=tpl-*.
	// GetSnapshotTemplateID prefers that over runtime.snapshot.id, which would
	// restore template snapshot_base with pause memory and panic the guest.
	delete(req.Annotations, constants.MasterAnnotationAppSnapshotTemplateID)
	return nil
}

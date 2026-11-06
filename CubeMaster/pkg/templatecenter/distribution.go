// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	imagev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
	"google.golang.org/protobuf/proto"
)

func buildReplicaForDistribution(target *node.Node, req *types.CreateCubeSandboxReq, artifactID, jobID string) ReplicaStatus {
	spec := ""
	instanceType := ""
	if req != nil {
		spec = calculateRequestSpec(req)
		instanceType = req.InstanceType
	}
	return ReplicaStatus{
		NodeID:          target.ID(),
		NodeIP:          target.HostIP(),
		InstanceType:    instanceType,
		Spec:            spec,
		Status:          ReplicaStatusFailed,
		Phase:           ReplicaPhaseDistributing,
		ArtifactID:      artifactID,
		LastJobID:       jobID,
		LastErrorPhase:  ReplicaPhaseDistributing,
		CleanupRequired: true,
	}
}

func cleanupArtifactOnNodes(ctx context.Context, artifactID, instanceType string, targets []*node.Node) error {
	if artifactID == "" {
		return nil
	}
	var cleanupErr error
	for _, target := range targets {
		if target == nil {
			continue
		}
		if _, err := destroyArtifactOnNode(ctx, artifactID, instanceType, target); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	return cleanupErr
}

// destroyArtifactOnNode issues an idempotent ext4 artifact destroy to a single
// node. It fills storage_media=ext4 and the instance-type annotation so the
// cubelet routes to its synchronous pmem destroy path (a plain DestroyImage is
// a no-op for ext4 artifacts that are not containerd images).
//
// Returns inUse=true (and nil error) when the node refuses because a running
// sandbox still references the artifact: that is a protection, not a failure,
// and the caller keeps the artifact for GC to retry. NotFound is treated as
// success (idempotent). Any other failure is returned as an error.
func destroyArtifactOnNode(ctx context.Context, artifactID, instanceType string, target *node.Node) (bool, error) {
	if target == nil {
		return false, nil
	}
	instanceType = strings.TrimSpace(instanceType)
	if instanceType == "" {
		log.G(ctx).Warnf("artifact cleanup: empty instanceType for artifact %s on node %s, defaulting to cubebox", artifactID, target.ID())
		instanceType = cubeboxv1.InstanceType_cubebox.String()
	}
	rsp, err := deleteImageOnCubelet(ctx, getCubeletAddrForDelete(target.HostIP()), &imagev1.DestroyImageRequest{
		RequestID: uuid.NewString(),
		Spec: &imagev1.ImageSpec{
			Image:        artifactID,
			StorageMedia: imagev1.ImageStorageMediaType_ext4.String(),
			Annotations: map[string]string{
				constants.CubeAnnotationsInsType: instanceType,
			},
		},
	})
	if err != nil {
		if isIgnorableArtifactDeleteError(err) {
			return false, nil
		}
		return false, fmt.Errorf("delete artifact %s on node %s: %w", artifactID, target.ID(), err)
	}
	if rsp.GetRet() != nil && int(rsp.GetRet().GetRetCode()) != int(errorcode.ErrorCode_Success) {
		if int(rsp.GetRet().GetRetCode()) == int(errorcode.ErrorCode_Conflict) {
			return true, nil // running sandbox still uses it; defer to GC
		}
		if isIgnorableArtifactDeleteMessage(rsp.GetRet().GetRetMsg()) {
			return false, nil
		}
		return false, fmt.Errorf("delete artifact %s on node %s failed: %s", artifactID, target.ID(), rsp.GetRet().GetRetMsg())
	}
	return false, nil
}

func cleanupTemplateReplicasOnNodes(ctx context.Context, templateID string, replicas []models.TemplateReplica, targets []*node.Node, backend string) error {
	if len(replicas) == 0 || len(targets) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(targets)*2)
	for _, target := range targets {
		if target == nil {
			continue
		}
		allowed[target.ID()] = struct{}{}
		if target.HostIP() != "" {
			allowed[target.HostIP()] = struct{}{}
		}
	}
	locators := make([]templateCleanupLocator, 0, len(replicas))
	for _, replica := range replicas {
		if _, ok := allowed[replica.NodeID]; !ok {
			if _, ok := allowed[replica.NodeIP]; !ok {
				continue
			}
		}
		locators = append(locators, templateCleanupLocator{
			NodeID: replica.NodeID,
			NodeIP: replica.NodeIP,
		})
	}
	if len(locators) == 0 {
		return nil
	}
	return cleanupTemplateReplicasWithLocators(ctx, templateID, locators, backend)
}

func distributeRootfsArtifact(ctx context.Context, req *types.CreateTemplateFromImageReq, generatedReq *types.CreateCubeSandboxReq, artifact *models.RootfsArtifact, templateID, jobID string) ([]*node.Node, int32, int32, int32, error) {
	if err := ensureArtifactDistributable(ctx, artifact); err != nil {
		return nil, 0, 0, 0, err
	}
	targets, err := resolveTemplateNodes(req.InstanceType, req.DistributionScope)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	backend := req.Backend
	if strings.TrimSpace(backend) == "" {
		backend = storageBackendFromCreate(generatedReq)
	}
	targets = templateBuildTargets(backend, targets)
	return distributeRootfsArtifactToNodes(ctx, req, generatedReq, artifact, templateID, jobID, targets)
}

// ensureArtifactDistributable refuses to push a CreateImage to cubelets when
// the artifact record is obviously incomplete. Without this guard the call
// proceeds with missing integrity metadata or an unusable ticket origin,
// which marks the template FAILED and masks the real build/publish error.
func ensureArtifactDistributable(ctx context.Context, artifact *models.RootfsArtifact) error {
	if artifact == nil {
		return fmt.Errorf("distributeRootfsArtifact: artifact is nil")
	}
	downloadBaseURL := effectiveArtifactDownloadBaseURL("", artifact)
	if artifact.Status != ArtifactStatusReady || artifact.Ext4SizeBytes == 0 || strings.TrimSpace(artifact.Ext4SHA256) == "" || downloadBaseURL == "" {
		return fmt.Errorf(
			"artifact %s is not ready for distribution (status=%s size_bytes=%d sha256_set=%t download_base_url=%q master_node_ip=%q); template build likely did not complete — check cubemaster logs for buildRootfsArtifact errors",
			artifact.ArtifactID, artifact.Status, artifact.Ext4SizeBytes,
			strings.TrimSpace(artifact.Ext4SHA256) != "", downloadBaseURL, artifact.MasterNodeIP,
		)
	}
	// The five checks above only read the artifact row. A row can be perfectly
	// READY while the ext4 it points at is not servable — either because the
	// artifact store did not survive a restart (issue #852) or because the
	// store is node-local and the pull would land on a node that never had
	// the file (issue #1005). Distributing either way hands every cubelet a
	// download URL that can only 404 or serve a stale file, so fail here with
	// the drift spelled out instead of collecting N identical per-node sha256
	// mismatches.
	//
	// Where the bytes live depends on the artifact backend: TC for fs/local
	// media, or the object store for S3. verifyArtifactServability probes the
	// same short-lived direct destination a Cubelet uses.
	return verifyArtifactServability(ctx, artifact)
}

// Injectable getter keeps config wiring testable without initializing global config.
var distributionConfig = config.GetConfig

// distributionWorkerCount returns the configured concurrency for one rootfs
// artifact distribution call, falling back to the built-in default when no
// config is loaded (e.g. unit tests).
func distributionWorkerCount() int {
	if cfg := distributionConfig(); cfg != nil && cfg.Common != nil && cfg.Common.DistributionWorkers > 0 {
		return cfg.Common.DistributionWorkers
	}
	return defaultDistributionWorkers
}

// distributeRootfsArtifactToNodes is distributeRootfsArtifact with an
// explicit target set, so the replica-backfill sweep can push an artifact to
// exactly the nodes that lack a READY replica instead of re-resolving the
// full healthy-node set. Callers must run ensureArtifactDistributable first.
func distributeRootfsArtifactToNodes(ctx context.Context, req *types.CreateTemplateFromImageReq, generatedReq *types.CreateCubeSandboxReq, artifact *models.RootfsArtifact, templateID, jobID string, targets []*node.Node) ([]*node.Node, int32, int32, int32, error) {
	annotations, err := artifactDownloadAnnotations(ctx, artifact, "")
	if err != nil {
		return nil, int32(len(targets)), 0, int32(len(targets)), err
	}
	annotations[constants.CubeAnnotationWritableLayerSize] = req.WritableLayerSize
	annotations[constants.CubeAnnotationTemplateSpecFingerprint] = artifact.TemplateSpecFingerprint
	annotations[constants.CubeAnnotationsInsType] = req.InstanceType
	spec := &imagev1.ImageSpec{
		Image:        artifact.ArtifactID,
		StorageMedia: imagev1.ImageStorageMediaType_ext4.String(),
		Annotations:  annotations,
	}
	expected := int32(len(targets))
	ready := int32(0)
	failed := int32(0)
	var firstErr error
	var lock sync.Mutex
	sem := make(chan struct{}, distributionWorkerCount())
	var wg sync.WaitGroup
	readyTargets := make([]*node.Node, 0, len(targets))
	for _, target := range targets {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			replica := buildReplicaForDistribution(target, generatedReq, artifact.ArtifactID, jobID)
			if templateID == "" || generatedReq == nil {
				err := errors.New("artifact distribution requires a persisted template replica before download")
				lock.Lock()
				failed++
				if firstErr == nil {
					firstErr = err
				}
				lock.Unlock()
				return
			}
			// Persist authorization before CreateImage. Cubelet asks Master for a
			// ticket during this RPC, so writing the replica afterwards creates a
			// deterministic 403 race.
			if err := UpsertReplica(ctx, templateID, generatedReq.InstanceType, replica); err != nil {
				lock.Lock()
				failed++
				if firstErr == nil {
					firstErr = fmt.Errorf("authorize artifact download on node %s: %w", target.ID(), err)
				}
				lock.Unlock()
				return
			}
			var err error
			refreshToken := ""
			if spec.Annotations[constants.CubeAnnotationRootfsArtifactTicketURL] != "" {
				refreshToken, err = BuildArtifactDownloadRefreshToken(ctx, artifact, target.ID(), time.Now())
			}
			if err != nil {
				lock.Lock()
				failed++
				replica.Phase = ReplicaPhaseFailed
				replica.ErrorMessage = err.Error()
				_ = UpsertReplica(ctx, templateID, generatedReq.InstanceType, replica)
				if firstErr == nil {
					firstErr = err
				}
				lock.Unlock()
				return
			}
			nodeSpec := proto.Clone(spec).(*imagev1.ImageSpec)
			if nodeSpec.Annotations == nil {
				nodeSpec.Annotations = make(map[string]string)
			}
			if refreshToken != "" {
				nodeSpec.Annotations[constants.CubeAnnotationRootfsArtifactRefreshToken] = refreshToken
			}
			rsp, err := cubelet.CreateImage(ctx, cubelet.GetCubeletAddr(target.HostIP()), &imagev1.CreateImageRequest{
				RequestID: uuid.New().String(),
				Spec:      nodeSpec,
			})
			lock.Lock()
			defer lock.Unlock()
			if err != nil {
				failed++
				replica.Phase = ReplicaPhaseFailed
				replica.ErrorMessage = err.Error()
				if firstErr == nil {
					firstErr = err
				}
				_ = UpsertReplica(ctx, templateID, generatedReq.InstanceType, replica)
				return
			}
			if rsp.GetRet() == nil || int(rsp.GetRet().GetRetCode()) != int(errorcode.ErrorCode_Success) {
				failed++
				replica.Phase = ReplicaPhaseFailed
				if firstErr == nil {
					if rsp.GetRet() != nil {
						firstErr = fmt.Errorf("cubelet create image on %s failed: %s", target.HostIP(), rsp.GetRet().GetRetMsg())
					} else {
						firstErr = fmt.Errorf("cubelet create image on %s returned empty ret", target.HostIP())
					}
				}
				if rsp.GetRet() != nil {
					replica.ErrorMessage = rsp.GetRet().GetRetMsg()
				} else {
					replica.ErrorMessage = "empty create image response"
				}
				_ = UpsertReplica(ctx, templateID, generatedReq.InstanceType, replica)
				return
			}
			// Record physical placement independently of the replica lifecycle so
			// last-owner-cleanup / GC can later reach this node even after the
			// replica row is removed (FIX-1).
			if err := upsertArtifactNodePlacement(ctx, artifact.ArtifactID, target.ID(), target.HostIP()); err != nil {
				failed++
				replica.Phase = ReplicaPhaseFailed
				replica.ErrorMessage = fmt.Sprintf("record artifact placement: %v", err)
				_ = UpsertReplica(ctx, templateID, generatedReq.InstanceType, replica)
				if firstErr == nil {
					firstErr = errors.New(replica.ErrorMessage)
				}
				return
			}
			replica.Phase = ReplicaPhaseDistributed
			replica.CleanupRequired = false
			replica.LastErrorPhase = ""
			replica.ErrorMessage = ""
			if err := UpsertReplica(ctx, templateID, generatedReq.InstanceType, replica); err != nil {
				failed++
				replica.Phase = ReplicaPhaseFailed
				replica.CleanupRequired = true
				replica.ErrorMessage = fmt.Sprintf("persist distributed replica: %v", err)
				if firstErr == nil {
					firstErr = errors.New(replica.ErrorMessage)
				}
				return
			}
			ready++
			readyTargets = append(readyTargets, target)
		}()
	}
	wg.Wait()
	return readyTargets, expected, ready, failed, firstErr
}

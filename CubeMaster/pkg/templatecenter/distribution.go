// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	imagev1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
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

func cleanupTemplateReplicasOnNodes(ctx context.Context, templateID string, replicas []models.TemplateReplica, targets []*node.Node) error {
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
	return cleanupTemplateReplicasWithLocators(ctx, templateID, locators)
}

func distributeRootfsArtifact(ctx context.Context, req *types.CreateTemplateFromImageReq, generatedReq *types.CreateCubeSandboxReq, artifact *models.RootfsArtifact, templateID, jobID string) ([]*node.Node, int32, int32, int32, error) {
	// Defense-in-depth: refuse to push a CreateImage to cubelets when the
	// artifact record is obviously incomplete. Without this guard the call
	// proceeds with ext4_size_bytes=0 / download_token=""; cubelet then
	// tries to pull with an empty token against a URL that falls back to
	// os.Hostname() (buildDownloadURL) and reports "invalid size:0", which
	// marks the template FAILED and masks the real cause (concurrent build
	// race; see artifactBuildLocks). Fail here with a clear diagnostic
	// instead.
	if artifact == nil {
		return nil, 0, 0, 0, fmt.Errorf("distributeRootfsArtifact: artifact is nil")
	}
	if artifact.Status != ArtifactStatusReady || artifact.Ext4SizeBytes == 0 || strings.TrimSpace(artifact.Ext4SHA256) == "" || strings.TrimSpace(artifact.DownloadToken) == "" || strings.TrimSpace(artifact.MasterNodeIP) == "" {
		return nil, 0, 0, 0, fmt.Errorf(
			"artifact %s is not ready for distribution (status=%s size_bytes=%d sha256_set=%t token_set=%t master_node_ip=%q); template build likely did not complete — check cubemaster logs for buildRootfsArtifact errors",
			artifact.ArtifactID, artifact.Status, artifact.Ext4SizeBytes,
			strings.TrimSpace(artifact.Ext4SHA256) != "", strings.TrimSpace(artifact.DownloadToken) != "", artifact.MasterNodeIP,
		)
	}
	targets, err := resolveTemplateNodes(req.InstanceType, req.DistributionScope)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	spec := &imagev1.ImageSpec{
		Image:        artifact.ArtifactID,
		StorageMedia: imagev1.ImageStorageMediaType_ext4.String(),
		Annotations: map[string]string{
			constants.CubeAnnotationRootfsArtifactID:        artifact.ArtifactID,
			constants.CubeAnnotationRootfsArtifactURL:       buildDownloadURL(artifact.MasterNodeIP, artifact.ArtifactID, artifact.DownloadToken),
			constants.CubeAnnotationRootfsArtifactToken:     artifact.DownloadToken,
			constants.CubeAnnotationRootfsArtifactSHA256:    artifact.Ext4SHA256,
			constants.CubeAnnotationRootfsArtifactSizeBytes: strconv.FormatInt(artifact.Ext4SizeBytes, 10),
			constants.CubeAnnotationWritableLayerSize:       req.WritableLayerSize,
			constants.CubeAnnotationTemplateSpecFingerprint: artifact.TemplateSpecFingerprint,
			constants.CubeAnnotationsInsType:                req.InstanceType,
		},
	}
	expected := int32(len(targets))
	ready := int32(0)
	failed := int32(0)
	var firstErr error
	var lock sync.Mutex
	sem := make(chan struct{}, defaultDistributionWorkers)
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
			rsp, err := cubelet.CreateImage(ctx, cubelet.GetCubeletAddr(target.HostIP()), &imagev1.CreateImageRequest{
				RequestID: uuid.New().String(),
				Spec:      spec,
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
				if templateID != "" && generatedReq != nil {
					_ = UpsertReplica(ctx, templateID, generatedReq.InstanceType, replica)
				}
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
				if templateID != "" && generatedReq != nil {
					_ = UpsertReplica(ctx, templateID, generatedReq.InstanceType, replica)
				}
				return
			}
			replica.Phase = ReplicaPhaseDistributed
			replica.CleanupRequired = false
			replica.LastErrorPhase = ""
			replica.ErrorMessage = ""
			ready++
			readyTargets = append(readyTargets, target)
			if templateID != "" && generatedReq != nil {
				_ = UpsertReplica(ctx, templateID, generatedReq.InstanceType, replica)
			}
			// Record physical placement independently of the replica lifecycle so
			// last-owner-cleanup / GC can later reach this node even after the
			// replica row is removed (FIX-1).
			if err := upsertArtifactNodePlacement(ctx, artifact.ArtifactID, target.ID(), target.HostIP()); err != nil {
				// Non-fatal: placement is a cleanup optimisation, not a
				// correctness gate for distribution. Backfill/GC reconcile later.
				log.G(ctx).Warnf("record artifact placement %s on node %s failed: %v", artifact.ArtifactID, target.ID(), err)
			}
		}()
	}
	wg.Wait()
	return readyTargets, expected, ready, failed, firstErr
}

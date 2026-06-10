// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	imagev1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"strconv"
	"sync"
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

func cleanupArtifactOnNodes(ctx context.Context, artifactID string, targets []*node.Node) error {
	if artifactID == "" {
		return nil
	}
	var cleanupErr error
	for _, target := range targets {
		if target == nil {
			continue
		}
		rsp, err := deleteImageOnCubelet(ctx, getCubeletAddrForDelete(target.HostIP()), &imagev1.DestroyImageRequest{
			RequestID: uuid.NewString(),
			Spec: &imagev1.ImageSpec{
				Image: artifactID,
			},
		})
		if err != nil {
			if isIgnorableArtifactDeleteError(err) {
				continue
			}
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete artifact %s on node %s: %w", artifactID, target.ID(), err))
			continue
		}
		if rsp.GetRet() != nil && int(rsp.GetRet().GetRetCode()) != int(errorcode.ErrorCode_Success) {
			if isIgnorableArtifactDeleteMessage(rsp.GetRet().GetRetMsg()) {
				continue
			}
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete artifact %s on node %s failed: %s", artifactID, target.ID(), rsp.GetRet().GetRetMsg()))
		}
	}
	return cleanupErr
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
		}()
	}
	wg.Wait()
	return readyTargets, expected, ready, failed, firstErr
}

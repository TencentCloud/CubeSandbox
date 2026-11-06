// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

// eagerAdoptJFSConcurrency bounds the parallel AdoptJFSTemplate fan-out.
var eagerAdoptJFSConcurrency = 4

// Seams so the fan-out is testable without live nodes.
var (
	eagerAdoptJFSResolveNodes = resolveTemplateNodes
	// eagerAdoptJFSNode performs one node's adoption end to end so tests
	// stub the whole hop (GetCubeletAddr needs cubelet config).
	eagerAdoptJFSNode = func(ctx context.Context, target *node.Node, templateID string) (*cubeboxv1.AdoptJFSTemplateResponse, error) {
		return cubelet.AdoptJFSTemplate(ctx, cubelet.GetCubeletAddr(target.HostIP()), &cubeboxv1.AdoptJFSTemplateRequest{
			RequestID:  uuid.New().String(),
			TemplateID: templateID,
		})
	}
)

func eagerAdoptJFSBackend(req *types.CreateTemplateFromImageReq, generatedReq *types.CreateCubeSandboxReq) bool {
	backend := ""
	if req != nil {
		backend = req.Backend
	}
	if strings.TrimSpace(backend) == "" {
		backend = storageBackendFromCreate(generatedReq)
	}
	return constants.IsJFSBackend(backend)
}

// eagerAdoptJFSTemplate pushes a freshly built jfs template to every
// in-scope node before the job is published READY, so the first
// sandbox create on a node does not pay the adoption (a copy_file_range
// metadata clone plus the small package-home copy) inside the request.
//
// Best-effort by design: failures are logged and never fail the template —
// the first-use adoption in prepareSharedTemplate remains the fallback for
// a node that was down, joined later, or failed here. Calling the builder
// is a cheap idempotent no-op (its local snapshot check short-circuits), so
// the fan-out simply covers the whole resolved target set.
// The owning job remains active until this synchronous fan-out completes.
// JFS deletes reject active jobs, so no additional adoption/delete fence is
// needed here.
func eagerAdoptJFSTemplate(ctx context.Context, req *types.CreateTemplateFromImageReq, generatedReq *types.CreateCubeSandboxReq, templateID string) {
	templateID = strings.TrimSpace(templateID)
	if templateID == "" {
		return
	}
	if !eagerAdoptJFSBackend(req, generatedReq) {
		return
	}
	instanceType := ""
	if req != nil {
		instanceType = req.InstanceType
	}
	if strings.TrimSpace(instanceType) == "" && generatedReq != nil {
		instanceType = generatedReq.InstanceType
	}
	var distributionScope []string
	if req != nil {
		distributionScope = req.DistributionScope
	}
	targets, err := eagerAdoptJFSResolveNodes(instanceType, distributionScope)
	if err != nil {
		log.G(ctx).Warnf("eager jfs adopt %s: resolve nodes: %v", templateID, err)
		return
	}
	if len(targets) == 0 {
		return
	}
	logger := log.G(ctx).WithFields(map[string]any{
		"template_id": templateID,
		"component":   "eager_jfs_adopt",
	})
	var adopted, failed int32
	sem := make(chan struct{}, eagerAdoptJFSConcurrency)
	var wg sync.WaitGroup
	for _, target := range targets {
		target := target
		if target == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rsp, callErr := eagerAdoptJFSNode(ctx, target, templateID)
			if callErr != nil {
				atomic.AddInt32(&failed, 1)
				logger.Warnf("eager jfs adopt on node %s failed: %v", target.ID(), callErr)
				return
			}
			if rsp.GetRet() == nil || int(rsp.GetRet().GetRetCode()) != int(errorcode.ErrorCode_Success) {
				atomic.AddInt32(&failed, 1)
				reason := "empty ret"
				if rsp.GetRet() != nil {
					reason = rsp.GetRet().GetRetMsg()
				}
				logger.Warnf("eager jfs adopt on node %s rejected: %s", target.ID(), reason)
				return
			}
			if rsp.GetAdopted() {
				atomic.AddInt32(&adopted, 1)
			}
		}()
	}
	wg.Wait()
	logger.Infof("eager jfs adopt: %d target(s), %d newly adopted, %d failed (first-use adoption is the fallback)",
		len(targets), adopted, failed)
}

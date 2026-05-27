// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter"
)

var (
	resolveSnapshotReadyNodeScopeFn = templatecenter.ResolveSnapshotReadyNodeScope
	resolveSnapshotReadyReplicaFn   = templatecenter.ResolveSnapshotReadyReplica
	resolveTemplateReadyReplicaFn   = templatecenter.ResolveTemplateReadyReplica
)

func getCubeboxReqTemplate() (*types.CreateCubeSandboxReq, error) {
	if config.GetConfig().ReqTemplateConf == nil || config.GetConfig().ReqTemplateConf.CubeBoxReqTemplate == "" {
		return nil, errors.New("cubebox instance type requires CubeBoxReqTemplate configuration")
	}

	templateReq := &types.CreateCubeSandboxReq{}
	err := utils.JSONTool.UnmarshalFromString(config.GetConfig().ReqTemplateConf.CubeBoxReqTemplate, templateReq)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal CubeBoxReqTemplate: %w", err)
	}

	return templateReq, nil
}

//go:noinline
func dealCubeboxReqTemplateByLocalConfig(ctx context.Context, reqInOut *types.CreateCubeSandboxReq) error {
	if reqInOut.InstanceType != cubebox.InstanceType_cubebox.String() {
		return nil
	}

	if config.GetConfig().ReqTemplateConf == nil || config.GetConfig().ReqTemplateConf.CubeBoxReqTemplate == "" {
		return errors.New("cubebox instance type requires CubeBoxReqTemplate configuration")
	}

	templateReq, err := getCubeboxReqTemplate()
	if err != nil {
		return fmt.Errorf("failed to unmarshal CubeBoxReqTemplate: %w", err)
	}

	if err := validateContainerRequirements(reqInOut); err != nil {
		return err
	}
	if err := validateTemplateRequirements(templateReq, reqInOut); err != nil {
		return err
	}

	dealVolumeTemplate(reqInOut.Volumes, templateReq.Volumes)

	for i, ctr := range reqInOut.Containers {
		if err := applyTemplateToContainer(ctr, templateReq.Containers[i], i); err != nil {
			return err
		}
	}

	applyTemplateAnnotationsAndLabels(templateReq, reqInOut)
	reqInOut.CubeVSContext = mergeCubeVSContexts(templateReq.CubeVSContext, reqInOut.CubeVSContext)

	if templateReq.NetworkType != "" {
		reqInOut.NetworkType = templateReq.NetworkType
	}

	log.G(ctx).Infof("Successfully dealCubeboxReqTemplateByLocalConfig: %s", utils.InterfaceToString(reqInOut))
	return nil
}

func validateContainerRequirements(req *types.CreateCubeSandboxReq) error {
	if len(req.Volumes) <= 0 {
		return errors.New("volume configuration is required")
	}
	if len(req.Containers) <= 0 {
		return errors.New("at least one container is required")
	}
	return nil
}

func validateTemplateRequirements(templateReq *types.CreateCubeSandboxReq, req *types.CreateCubeSandboxReq) error {
	if len(templateReq.Containers) < len(req.Containers) {
		return fmt.Errorf("template containers count (%d) is less than request containers count (%d)",
			len(templateReq.Containers), len(req.Containers))
	}
	return nil
}

func applyTemplateToContainer(ctr *types.Container, templateCtr *types.Container, index int) error {
	if ctr.Name == "" {
		ctr.Name = templateCtr.Name
		if ctr.Name == "" {
			ctr.Name = "cubebox_" + strconv.Itoa(index)
		}
	}

	if ctr.Image == nil {
		ctr.Image = &types.ImageSpec{}
	}
	applyTemplateImageSpec(templateCtr.Image, ctr.Image)

	if ctr.Resources == nil {
		ctr.Resources = &types.Resource{}
	}
	applyTemplateResources(templateCtr.Resources, ctr.Resources)

	ctr.Syscalls = templateCtr.Syscalls
	ctr.Sysctls = templateCtr.Sysctls
	ctr.SecurityContext = templateCtr.SecurityContext

	ctr.Envs = append(ctr.Envs, templateCtr.Envs...)
	applyTemplateVolumeMounts(templateCtr, ctr)

	if !isContainerReqWhiteTag("WorkingDir") {
		ctr.WorkingDir = templateCtr.WorkingDir
	}

	if !isContainerReqWhiteTag("RLimit") {
		ctr.RLimit = templateCtr.RLimit
	}
	if !isContainerReqWhiteTag("DnsConfig") {
		ctr.DnsConfig = templateCtr.DnsConfig
	}
	if !isContainerReqWhiteTag("HostAliases") {
		ctr.HostAliases = templateCtr.HostAliases
	}
	if !isContainerReqWhiteTag("Poststop") {
		ctr.Poststop = templateCtr.Poststop
	}
	if !isContainerReqWhiteTag("Prestop") {
		ctr.Prestop = templateCtr.Prestop
	}

	return nil
}

func applyTemplateVolumeMounts(templateCtr *types.Container, ctr *types.Container) {

	existNames := make(map[string]struct{})
	existPaths := make(map[string]struct{})
	for _, vm := range ctr.VolumeMounts {
		if vm == nil {
			continue
		}
		if vm.Name != "" {
			existNames[vm.Name] = struct{}{}
		}
		if vm.ContainerPath != "" {
			existPaths[vm.ContainerPath] = struct{}{}
		}
	}

	for _, vm := range templateCtr.VolumeMounts {
		if vm == nil {
			continue
		}
		_, nameExist := existNames[vm.Name]
		_, pathExist := existPaths[vm.ContainerPath]
		if !nameExist && !pathExist {
			ctr.VolumeMounts = append(ctr.VolumeMounts, vm)
			if vm.Name != "" {
				existNames[vm.Name] = struct{}{}
			}
			if vm.ContainerPath != "" {
				existPaths[vm.ContainerPath] = struct{}{}
			}
		}
	}
}

func applyTemplateResources(resourceIn *types.Resource, resourceOut *types.Resource) {
	if resourceIn == nil {
		return
	}
	if resourceOut == nil {
		resourceOut = &types.Resource{}
	}
	if resourceIn.Cpu != "" {
		resourceOut.Cpu = resourceIn.Cpu
	}
	if resourceIn.Mem != "" {
		resourceOut.Mem = resourceIn.Mem
	}
	if resourceIn.Limit != nil {
		resourceOut.Limit = resourceIn.Limit
	}
}

func applyTemplateImageSpec(imageSpecIn *types.ImageSpec, imageSpecOut *types.ImageSpec) {
	if imageSpecIn == nil {
		return
	}
	if imageSpecOut == nil {

		return
	}
	if imageSpecOut.StorageMedia == "" {

		imageSpecOut.StorageMedia = imageSpecIn.StorageMedia
	}

	if imageSpecIn.Image != "" {
		imageSpecOut.Image = imageSpecIn.Image
	}
	if imageSpecIn.Token != "" {
		imageSpecOut.Token = imageSpecIn.Token
	}
	if imageSpecIn.Name != "" {
		imageSpecOut.Name = imageSpecIn.Name
	}
	if imageSpecIn.Annotations != nil {
		if imageSpecOut.Annotations == nil {
			imageSpecOut.Annotations = make(map[string]string)
		}
		maps.Copy(imageSpecOut.Annotations, imageSpecIn.Annotations)
	}
}

//go:noinline
func applyTemplateAnnotationsAndLabels(reqIn *types.CreateCubeSandboxReq, reqOut *types.CreateCubeSandboxReq) {
	if reqIn.Annotations != nil {
		if reqOut.Annotations == nil {
			reqOut.Annotations = make(map[string]string)
		}
		for k, v := range reqIn.Annotations {
			if k == constants.AnnotationsNetID {
				if _, ok := reqOut.Annotations[constants.AnnotationsNetID]; ok {

					continue
				}
			}
			reqOut.Annotations[k] = v
		}
	}

	if reqIn.Labels != nil {
		if reqOut.Labels == nil {
			reqOut.Labels = make(map[string]string)
		}
		maps.Copy(reqOut.Labels, reqIn.Labels)
	}
}

func mergeCubeVSContexts(templateCtx *types.CubeVSContext, requestCtx *types.CubeVSContext) *types.CubeVSContext {
	switch {
	case templateCtx == nil:
		return cloneCubeVSContext(requestCtx)
	case requestCtx == nil:
		return cloneCubeVSContext(templateCtx)
	}

	out := cloneCubeVSContext(templateCtx)
	if requestCtx.AllowInternetAccess != nil {
		allowInternetAccess := *requestCtx.AllowInternetAccess
		out.AllowInternetAccess = &allowInternetAccess
	}
	if len(requestCtx.AllowOut) > 0 {
		out.AllowOut = appendUniqueCIDRs(out.AllowOut, requestCtx.AllowOut)
	}
	if len(requestCtx.DenyOut) > 0 {
		out.DenyOut = appendUniqueCIDRs(out.DenyOut, requestCtx.DenyOut)
	}
	return out
}

func cloneCubeVSContext(in *types.CubeVSContext) *types.CubeVSContext {
	if in == nil {
		return nil
	}
	out := &types.CubeVSContext{
		AllowOut: append([]string(nil), in.AllowOut...),
		DenyOut:  append([]string(nil), in.DenyOut...),
	}
	if in.AllowInternetAccess != nil {
		allowInternetAccess := *in.AllowInternetAccess
		out.AllowInternetAccess = &allowInternetAccess
	}
	return out
}

func appendUniqueCIDRs(base []string, extra []string) []string {
	seen := make(map[string]struct{}, len(base)+len(extra))
	out := append([]string(nil), base...)
	for _, cidr := range base {
		seen[cidr] = struct{}{}
	}
	for _, cidr := range extra {
		if cidr == "" {
			continue
		}
		if _, ok := seen[cidr]; ok {
			continue
		}
		seen[cidr] = struct{}{}
		out = append(out, cidr)
	}
	return out
}

func isContainerReqWhiteTag(tag string) bool {
	if config.GetConfig().ReqTemplateConf == nil || config.GetConfig().ReqTemplateConf.WhitelistReqTag == nil {
		return false
	}
	whitelistReqTag := config.GetConfig().ReqTemplateConf.WhitelistReqTag
	_, ok := whitelistReqTag[tag]
	return ok
}

//go:noinline
func dealCubeboxCreateReqWithTemplate(ctx context.Context, reqInOut *types.CreateCubeSandboxReq) error {

	if reqInOut.InstanceType != cubebox.InstanceType_cubebox.String() {
		return nil
	}
	constants.NormalizeAppSnapshotAnnotations(reqInOut.Annotations)

	templateID, hasTemplateID := reqInOut.Annotations[constants.CubeAnnotationAppSnapshotTemplateID]

	if !hasTemplateID && config.GetConfig().Common.EnableAGSColdStartSwitch {
		return handleColdStartCompatibility(reqInOut)
	}

	if constants.GetAppSnapshotVersion(reqInOut.Annotations) == templatecenter.DefaultTemplateVersion {
		return dealCubeboxCreateReqWithTemplateCenter(ctx, templateID, reqInOut)
	}

	return dealCubeboxReqTemplateByLocalConfig(ctx, reqInOut)
}

func handleColdStartCompatibility(reqInOut *types.CreateCubeSandboxReq) error {

	if _, hasNetID := reqInOut.Annotations[constants.AnnotationsNetID]; hasNetID {
		return nil
	}

	if reqInOut.Annotations == nil {
		reqInOut.Annotations = make(map[string]string)
	}

	templateReq, err := getCubeboxReqTemplate()
	if err != nil {
		return fmt.Errorf("failed to unmarshal CubeBoxReqTemplate: %w", err)
	}
	netID, ok := templateReq.Annotations[constants.AnnotationsNetID]
	if !ok {
		return errors.New("netID is missing in CubeBoxReqTemplate")
	}
	reqInOut.Annotations[constants.AnnotationsNetID] = netID
	return nil
}

//go:noinline
func dealCubeboxCreateReqWithTemplateCenter(ctx context.Context, templateID string, reqInOut *types.CreateCubeSandboxReq) error {
	start := time.Now()
	defer func() {
		templatecenter.ReportResolveMetric(ctx, time.Since(start))
	}()
	if templateID == "" {
		return errors.New("templateID is empty")
	}
	stageStart := time.Now()
	templateReq, err := templatecenter.GetTemplateRequest(ctx, templateID)
	templatecenter.ReportResolveStageMetric(ctx, constants.ActionTemplateResolveRequest, time.Since(stageStart))
	if err != nil {
		return fmt.Errorf("failed to get template param from store: %w", err)
	}
	constants.NormalizeAppSnapshotAnnotations(templateReq.Annotations)
	stageStart = time.Now()
	err = templatecenter.EnsureTemplateLocalityReady(ctx, templateID, reqInOut.InstanceType)
	templatecenter.ReportResolveStageMetric(ctx, constants.ActionTemplateResolveLocality, time.Since(stageStart))
	if err != nil {
		return fmt.Errorf("template %s is not ready on any healthy node: %w", templateID, err)
	}
	stageStart = time.Now()
	templateKind, err := templatecenter.GetTemplateKind(ctx, templateID)
	templatecenter.ReportResolveStageMetric(ctx, constants.ActionTemplateResolveKind, time.Since(stageStart))
	if err != nil {
		return fmt.Errorf("failed to resolve template kind: %w", err)
	}
	if resolved := templateResolveResultFromContext(ctx); resolved != nil {
		resolved.TemplateID = templateID
		resolved.Kind = templateKind
	}
	bindStart := time.Now()
	defer func() {
		templatecenter.ReportResolveStageMetric(ctx, constants.ActionTemplateResolveBind, time.Since(bindStart))
	}()
	if strings.EqualFold(templateKind, templatecenter.TemplateKindSnapshot) {
		if err := bindSnapshotCreateReplica(ctx, templateID, reqInOut); err != nil {
			return err
		}
	}
	if log.IsDebug() {
		log.G(ctx).Debugf("getTemplateParam success:%s", utils.InterfaceToString(templateReq))
	} else {
		log.G(ctx).Infof("getTemplateParam success:template=%s %s", templateID, summarizeTemplateRequest(templateReq))
	}

	applyTemplateAnnotationsAndLabels(templateReq, reqInOut)
	if !strings.EqualFold(templateKind, templatecenter.TemplateKindSnapshot) {
		if err := bindAppSnapshotTemplateReplica(ctx, templateID, reqInOut); err != nil {
			return err
		}
	}
	reqInOut.CubeVSContext = mergeCubeVSContexts(templateReq.CubeVSContext, reqInOut.CubeVSContext)

	reqInOut.Volumes = append(reqInOut.Volumes, templateReq.Volumes...)

	for i, templateCtr := range templateReq.Containers {
		if len(reqInOut.Containers) <= i {

			reqInOut.Containers = append(reqInOut.Containers, templateCtr)
			continue
		}
		if err := applyTemplateToContainer(reqInOut.Containers[i], templateCtr, i); err != nil {
			return err
		}
	}

	if templateReq.NetworkType != "" {
		reqInOut.NetworkType = templateReq.NetworkType
	}
	if templateReq.RuntimeHandler != "" {
		reqInOut.RuntimeHandler = templateReq.RuntimeHandler
	}
	if templateReq.Namespace != "" {
		reqInOut.Namespace = templateReq.Namespace
	}
	if reqInOut.Labels == nil {
		reqInOut.Labels = map[string]string{}
	}
	if reqInOut.Annotations != nil && reqInOut.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] != "" {
		reqInOut.Labels[constants.CubeAnnotationAppSnapshotTemplateID] = reqInOut.Annotations[constants.CubeAnnotationAppSnapshotTemplateID]
	}
	if log.IsDebug() {
		log.G(ctx).Debugf("dealCubeboxCreateReqWithTemplateCenter success:%s", utils.InterfaceToString(reqInOut))
	} else {
		log.G(ctx).Infof("dealCubeboxCreateReqWithTemplateCenter success:template=%s %s", templateID, summarizeTemplateRequest(reqInOut))
	}
	return nil
}

func constrainSnapshotCreateScope(ctx context.Context, snapshotID string, reqInOut *types.CreateCubeSandboxReq) error {
	readyScope, err := resolveSnapshotReadyNodeScopeFn(ctx, snapshotID)
	if err != nil {
		return fmt.Errorf("snapshot %s has no ready local replica scope: %w", snapshotID, err)
	}
	scopeSet := make(map[string]struct{}, len(readyScope))
	for _, item := range readyScope {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		scopeSet[item] = struct{}{}
	}
	if len(reqInOut.DistributionScope) == 0 {
		reqInOut.DistributionScope = readyScope
		return nil
	}
	filtered := make([]string, 0, len(reqInOut.DistributionScope))
	for _, item := range reqInOut.DistributionScope {
		item = strings.TrimSpace(item)
		if _, ok := scopeSet[item]; ok {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) == 0 {
		return fmt.Errorf("snapshot %s is only ready on nodes %v, requested distribution_scope=%v", snapshotID, readyScope, reqInOut.DistributionScope)
	}
	reqInOut.DistributionScope = filtered
	return nil
}

// bindSnapshotCreateReplica selects a node to host a new sandbox restored
// from snapshot and stamps only the logical id annotations onto the request.
//
// v4 contract: master MUST NOT carry physical volume references for
// snapshots. cubelet resolves memory_vol/rootfs_vol from its local catalog
// keyed by RuntimeSnapshotID. The legacy memory_vol/memory_dev annotation
// keys are explicitly deleted so any stale value supplied by the caller
// cannot reach the cubelet.
func bindSnapshotCreateReplica(ctx context.Context, snapshotID string, reqInOut *types.CreateCubeSandboxReq) error {
	if err := constrainSnapshotCreateScope(ctx, snapshotID, reqInOut); err != nil {
		return err
	}
	preferredNodeID := preferredDistributionNodeID(reqInOut)
	replica, err := resolveSnapshotReadyReplicaFn(ctx, snapshotID, preferredNodeID)
	if err != nil {
		return fmt.Errorf("snapshot %s has no bindable ready replica: %w", snapshotID, err)
	}
	if resolved := templateResolveResultFromContext(ctx); resolved != nil {
		resolved.ChosenReplica = replica
		resolved.HasChosenReplica = true
	}
	selectedNodeID := strings.TrimSpace(replica.NodeID)
	if selectedNodeID == "" {
		selectedNodeID = preferredNodeID
	}
	if selectedNodeID != "" {
		reqInOut.DistributionScope = []string{selectedNodeID}
	}
	if reqInOut.Annotations == nil {
		reqInOut.Annotations = map[string]string{}
	}
	reqInOut.Annotations[constants.CubeAnnotationRuntimeSnapshotID] = strings.TrimSpace(snapshotID)
	reqInOut.Annotations[constants.CubeAnnotationRuntimeSnapshotAttachedAt] = time.Now().UTC().Format(time.RFC3339Nano)
	return nil
}

// bindAppSnapshotTemplateReplica selects a node to host a new sandbox
// restored from an AppSnapshot template. Only the logical id annotation is
// carried on the request (set upstream by applyTemplateAnnotationsAndLabels);
// cubelet resolves memory_vol/memory_kind/rootfs_vol from its local catalog
// keyed by CubeAnnotationAppSnapshotTemplateID. v5: the legacy physical
// memory_vol/memory_kind annotation keys no longer exist as constants.
func bindAppSnapshotTemplateReplica(ctx context.Context, templateID string, reqInOut *types.CreateCubeSandboxReq) error {
	preferredNodeID := preferredDistributionNodeID(reqInOut)
	if _, err := resolveTemplateReadyReplicaFn(ctx, templateID, preferredNodeID); err != nil {
		return fmt.Errorf("template %s has no bindable ready replica: %w", templateID, err)
	}
	if reqInOut.Annotations == nil {
		reqInOut.Annotations = map[string]string{}
	}
	return nil
}

func preferredDistributionNodeID(req *types.CreateCubeSandboxReq) string {
	if req == nil || len(req.DistributionScope) == 0 {
		return ""
	}
	return strings.TrimSpace(req.DistributionScope[0])
}

func summarizeTemplateRequest(req *types.CreateCubeSandboxReq) string {
	if req == nil {
		return "request=nil"
	}
	return fmt.Sprintf(
		"containers=%d volumes=%d labels=%d annotations=%d network=%s runtime=%s namespace=%s cubevs_context=%s",
		len(req.Containers),
		len(req.Volumes),
		len(req.Labels),
		len(req.Annotations),
		req.NetworkType,
		req.RuntimeHandler,
		req.Namespace,
		formatCubeVSContextSummary(req.CubeVSContext),
	)
}

func formatCubeVSContextSummary(ctx *types.CubeVSContext) string {
	if ctx == nil {
		return "allow_internet_access=default(true) allow_out=[] deny_out=[]"
	}
	allowInternetAccess := "default(true)"
	if ctx.AllowInternetAccess != nil {
		allowInternetAccess = fmt.Sprintf("%t", *ctx.AllowInternetAccess)
	}
	return fmt.Sprintf("allow_internet_access=%s allow_out=%v deny_out=%v", allowInternetAccess, ctx.AllowOut, ctx.DenyOut)
}

func dealVolumeTemplate(volumes []*types.Volume, templateVolumes []*types.Volume) {
	for _, v := range volumes {
		if v == nil || v.VolumeSource == nil || v.VolumeSource.EmptyDir == nil {
			continue
		}
		if v.Name != "" || v.VolumeSource.EmptyDir.Medium != 0 {
			continue
		}
		templateV := getTemplateVolumes(v.VolumeSource.EmptyDir, templateVolumes)
		if templateV == nil || templateV.VolumeSource == nil || templateV.VolumeSource.EmptyDir == nil {
			continue
		}
		v.Name = templateV.Name
		v.VolumeSource.EmptyDir.Medium = templateV.VolumeSource.EmptyDir.Medium
	}
}

func getTemplateVolumes(sourceVolume interface{}, templateVolumes []*types.Volume) *types.Volume {
	for _, templateVolume := range templateVolumes {
		if templateVolume == nil || templateVolume.VolumeSource == nil {
			continue
		}
		templateSource := templateVolume.VolumeSource
		switch v := sourceVolume.(type) {
		case *types.EmptyDirVolumeSource:
			if v != nil && templateSource.EmptyDir != nil {
				return templateVolume
			}
		case *types.HostDirVolumeSources:
			if v != nil && templateSource.HostDirVolumeSources != nil {
				return templateVolume
			}
		case *types.SandboxPathVolumeSource:
			if v != nil && templateSource.SandboxPath != nil {
				return templateVolume
			}
		}
	}
	return nil
}

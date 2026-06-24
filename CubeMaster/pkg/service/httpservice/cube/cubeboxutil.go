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
	reqInOut.CubeNetworkConfig = mergeCubeNetworkConfigs(templateReq.CubeNetworkConfig, reqInOut.CubeNetworkConfig)

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

func mergeCubeNetworkConfigs(templateCfg *types.CubeNetworkConfig, requestCfg *types.CubeNetworkConfig) *types.CubeNetworkConfig {
	switch {
	case templateCfg == nil:
		return cloneCubeNetworkConfig(requestCfg)
	case requestCfg == nil:
		return cloneCubeNetworkConfig(templateCfg)
	}

	out := cloneCubeNetworkConfig(templateCfg)
	if requestCfg.AllowInternetAccess != nil {
		allowInternetAccess := *requestCfg.AllowInternetAccess
		out.AllowInternetAccess = &allowInternetAccess
	}
	// AllowPublicTraffic: per-create override wins over the template. Templates
	// rarely set this; the request side carries the user's explicit decision
	// (e2b SDK shape: network.allowPublicTraffic). Without this the request's
	// false would silently fall back to the template's (usually nil) value and
	// CubeProxy would never enforce the gate.
	if requestCfg.AllowPublicTraffic != nil {
		allowPublicTraffic := *requestCfg.AllowPublicTraffic
		out.AllowPublicTraffic = &allowPublicTraffic
	}
	if len(requestCfg.AllowOut) > 0 {
		out.AllowOut = appendUniqueCIDRs(out.AllowOut, requestCfg.AllowOut)
	}
	if len(requestCfg.DenyOut) > 0 {
		out.DenyOut = appendUniqueCIDRs(out.DenyOut, requestCfg.DenyOut)
	}
	if len(requestCfg.Rules) > 0 {
		out.Rules = mergeEgressRules(out.Rules, requestCfg.Rules)
	}
	return out
}

func cloneCubeNetworkConfig(in *types.CubeNetworkConfig) *types.CubeNetworkConfig {
	if in == nil {
		return nil
	}
	out := &types.CubeNetworkConfig{
		AllowOut: append([]string(nil), in.AllowOut...),
		DenyOut:  append([]string(nil), in.DenyOut...),
		Rules:    cloneEgressRules(in.Rules),
	}
	if in.AllowInternetAccess != nil {
		allowInternetAccess := *in.AllowInternetAccess
		out.AllowInternetAccess = &allowInternetAccess
	}
	if in.AllowPublicTraffic != nil {
		allowPublicTraffic := *in.AllowPublicTraffic
		out.AllowPublicTraffic = &allowPublicTraffic
	}
	return out
}

// mergeEgressRules combines template + request rules. Rules sharing the same
// Name are overridden by the request side; otherwise request rules are
// appended after template rules to preserve first-match-wins ordering with
// the template's policy taking precedence on overlap.
func mergeEgressRules(base []*types.EgressRule, extra []*types.EgressRule) []*types.EgressRule {
	if len(extra) == 0 {
		return base
	}
	indexByName := make(map[string]int, len(base))
	out := cloneEgressRules(base)
	for i, r := range out {
		if r != nil {
			indexByName[r.Name] = i
		}
	}
	for _, r := range extra {
		if r == nil {
			continue
		}
		cloned := cloneEgressRule(r)
		if idx, ok := indexByName[r.Name]; ok {
			out[idx] = cloned
			continue
		}
		indexByName[r.Name] = len(out)
		out = append(out, cloned)
	}
	return out
}

func cloneEgressRules(in []*types.EgressRule) []*types.EgressRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]*types.EgressRule, 0, len(in))
	for _, r := range in {
		out = append(out, cloneEgressRule(r))
	}
	return out
}

func cloneEgressRule(in *types.EgressRule) *types.EgressRule {
	if in == nil {
		return nil
	}
	out := &types.EgressRule{Name: in.Name}
	if in.Match != nil {
		match := *in.Match
		match.Method = append([]string(nil), in.Match.Method...)
		out.Match = &match
	}
	if in.Action != nil {
		action := &types.EgressRuleAction{Allow: in.Action.Allow}
		if in.Action.Audit != nil {
			audit := *in.Action.Audit
			action.Audit = &audit
		}
		if len(in.Action.Inject) > 0 {
			action.Inject = make([]*types.EgressRuleInject, 0, len(in.Action.Inject))
			for _, inj := range in.Action.Inject {
				if inj == nil {
					continue
				}
				cp := *inj
				if inj.Format != nil {
					format := *inj.Format
					cp.Format = &format
				}
				action.Inject = append(action.Inject, &cp)
			}
		}
		out.Action = action
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
	reqInOut.CubeNetworkConfig = mergeCubeNetworkConfigs(templateReq.CubeNetworkConfig, reqInOut.CubeNetworkConfig)

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
		"containers=%d volumes=%d labels=%d annotations=%d network=%s runtime=%s namespace=%s cube_network_config=%s",
		len(req.Containers),
		len(req.Volumes),
		len(req.Labels),
		len(req.Annotations),
		req.NetworkType,
		req.RuntimeHandler,
		req.Namespace,
		formatCubeNetworkConfigSummary(req.CubeNetworkConfig),
	)
}

func formatCubeNetworkConfigSummary(cfg *types.CubeNetworkConfig) string {
	if cfg == nil {
		return "allow_internet_access=default(true) allow_out=[] deny_out=[] rules=0"
	}
	allowInternetAccess := "default(true)"
	if cfg.AllowInternetAccess != nil {
		allowInternetAccess = fmt.Sprintf("%t", *cfg.AllowInternetAccess)
	}
	return fmt.Sprintf("allow_internet_access=%s allow_out=%v deny_out=%v rules=%d", allowInternetAccess, cfg.AllowOut, cfg.DenyOut, len(cfg.Rules))
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

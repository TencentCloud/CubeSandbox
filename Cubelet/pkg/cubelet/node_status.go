// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubelet

import (
	"bufio"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	imagestore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	cubeletnodemeta "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet/nodemeta"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet/nodestatus"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet/resourcesource"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/masterclient"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/networkagentclient"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	cloudproviderapi "k8s.io/cloud-provider/api"
	"k8s.io/klog/v2"
	kubeletapis "k8s.io/kubelet/pkg/apis"
	taintutil "k8s.io/kubernetes/pkg/util/taints"
)

const (
	defaultHostQuotaCPUOvercommitFactor = 2
	defaultHostQuotaMemNumerator        = 5
	defaultHostQuotaMemDenominator      = 4
	defaultHostMemPerMVMMB              = 512
)

var (
	hostCPUCount = func() int {
		return goruntime.NumCPU()
	}
	readHostMemoryTotalMB = detectHostMemoryTotalMB
)

func (kl *Cubelet) registerWithAPIServer() {
	if kl.registrationCompleted || kl.masterClient == nil {
		return
	}

	step := 100 * time.Millisecond
	for {
		time.Sleep(step)
		step *= 2
		if step >= 7*time.Second {
			step = 7 * time.Second
		}

		node, err := kl.initialNode(context.TODO())
		if err != nil {
			klog.ErrorS(err, "Unable to construct node metadata for cubelet")
			continue
		}
		klog.InfoS("Attempting to register node", "node", node.Name)
		if kl.tryRegisterWithAPIServer(node) {
			klog.InfoS("Successfully registered node", "node", node.Name)
			kl.registrationCompleted = true
			return
		}
	}
}

func (kl *Cubelet) reconcileExtendedResource(initialNode, node *cubeletnodemeta.Node) bool {
	return updateDefaultResources(initialNode, node)
}

func (kl *Cubelet) tryRegisterWithAPIServer(node *cubeletnodemeta.Node) bool {
	if kl.masterClient == nil {
		return false
	}
	if err := kl.masterClient.RegisterNode(context.TODO(), kl.buildRegisterRequest(node)); err != nil {
		klog.ErrorS(err, "Unable to register node with CubeMaster", "node", node.Name)
		return false
	}
	kl.lastNodeSnapshot = node.DeepCopy()
	return true
}

func (kl *Cubelet) setNodeStatus(ctx context.Context, node *cubeletnodemeta.Node) {
	for i, f := range kl.SetNodeStatusFuncs {
		klog.V(5).InfoS("Setting node status condition code", "position", i, "node", node.Name)
		if err := f(ctx, node); err != nil {
			klog.ErrorS(err, "Failed to set some node status fields", "node", node.Name)
		}
	}
}

func (kl *Cubelet) defaultNodeStatusFuncs() []func(context.Context, *cubeletnodemeta.Node) error {
	var setters []func(ctx context.Context, n *cubeletnodemeta.Node) error

	var maxImages int32 = 50
	if cfg := getMetaConfig(); cfg != nil && cfg.NodeStatusMaxImages != 0 {
		maxImages = cfg.NodeStatusMaxImages
	}

	setters = append(setters,
		nodestatus.NodeAddress(kl.nodeIPs, kl.hostname, true),
		nodestatus.Images(maxImages, func(ctx context.Context) ([]imagestore.Image, error) {
			if kl.criImage != nil {
				return kl.criImage.ListImage(ctx)
			}
			return []imagestore.Image{}, nil
		}),
		nodestatus.LocalTemplate(kl.rtManager.ListLocalTemplates),
	)

	setters = append(setters, ReadyCondition(
		kl.clock.Now,
		kl.runtimeErrorsFunc,
		kl.networkErrorsFunc,
		kl.storageErrorsFunc,
		kl.nodeShutdownManagerErrorsFunc,
		kl.recordEventFunc,
		false,
	))
	return setters
}

func (kl *Cubelet) runtimeErrorsFunc() error { return nil }
func (kl *Cubelet) storageErrorsFunc() error { return nil }
func (kl *Cubelet) networkErrorsFunc() error {
	cfg := config.GetConfig()
	if cfg == nil || cfg.Common == nil || !cfg.Common.EnableNetworkAgent {
		return nil
	}
	if kl.networkAgentClient == nil {
		return fmt.Errorf("network-agent client is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return kl.networkAgentClient.Health(ctx, &networkagentclient.HealthRequest{})
}
func (kl *Cubelet) nodeShutdownManagerErrorsFunc() error           { return nil }
func (kl *Cubelet) recordEventFunc(eventType string, event string) {}

func (kl *Cubelet) initialNode(ctx context.Context) (*cubeletnodemeta.Node, error) {
	node := &cubeletnodemeta.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: string(kl.nodeName),
			Labels: map[string]string{
				corev1.LabelHostname:   string(kl.nodeName),
				corev1.LabelOSStable:   goruntime.GOOS,
				corev1.LabelArchStable: goruntime.GOARCH,
				kubeletapis.LabelOS:    goruntime.GOOS,
				kubeletapis.LabelArch:  goruntime.GOARCH,
			},
		},
		Status: cubeletnodemeta.NodeStatus{
			Capacity:    map[corev1.ResourceName]resource.Quantity{},
			Allocatable: map[corev1.ResourceName]resource.Quantity{},
		},
	}
	osLabels, err := getOSSpecificLabels()
	if err != nil {
		return nil, err
	}
	for label, value := range osLabels {
		node.Labels[label] = value
	}

	var nodeTaints []corev1.Taint
	unschedulableTaint := corev1.Taint{Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule}
	if node.Spec.Unschedulable && !taintutil.TaintExists(nodeTaints, &unschedulableTaint) {
		nodeTaints = append(nodeTaints, unschedulableTaint)
	}
	if kl.externalCloudProvider {
		nodeTaints = append(nodeTaints, corev1.Taint{
			Key:    cloudproviderapi.TaintExternalCloudProvider,
			Value:  "true",
			Effect: corev1.TaintEffectNoSchedule,
		})
	}
	if len(nodeTaints) > 0 {
		node.Spec.Taints = nodeTaints
	}
	for k, v := range kl.NodeLabels {
		node.ObjectMeta.Labels[k] = v
	}
	if kl.providerID != "" {
		node.Spec.ProviderID = kl.providerID
	}
	if kl.instanceType != "" {
		node.Labels[corev1.LabelInstanceType] = kl.instanceType
		node.Spec.InstanceType = kl.instanceType
	}
	applyHostQuota(node)
	kl.setNodeStatus(ctx, node)
	return node, nil
}

func (kl *Cubelet) fastStatusUpdateOnce() {
	ctx := context.Background()
	start := time.Now()
	stopCh := make(chan struct{})
	wait.Until(func() {
		if kl.fastNodeStatusUpdate(ctx, time.Since(start) >= nodeReadyGracePeriod) {
			close(stopCh)
		}
	}, 1*time.Second, stopCh)
}

func (kl *Cubelet) fastNodeStatusUpdate(ctx context.Context, timeout bool) (completed bool) {
	kl.syncNodeStatusMux.Lock()
	defer func() {
		kl.syncNodeStatusMux.Unlock()
		if completed {
			kl.updateRuntimeMux.Lock()
			defer kl.updateRuntimeMux.Unlock()
			kl.containerRuntimeReadyExpected = true
		}
	}()

	if timeout {
		klog.ErrorS(nil, "Node not becoming ready in time after startup")
		return true
	}

	originalNode, err := kl.GetNode()
	if err != nil {
		klog.ErrorS(err, "Error getting the current node")
		return false
	}
	readyIdx, originalReady := cubeletnodemeta.GetNodeCondition(&originalNode.Status, corev1.NodeReady)
	if readyIdx == -1 {
		return false
	}
	if originalReady.Status == corev1.ConditionTrue {
		return true
	}
	node, changed := kl.updateNode(ctx, originalNode)
	if !changed {
		return false
	}
	readyIdx, ready := cubeletnodemeta.GetNodeCondition(&node.Status, corev1.NodeReady)
	if readyIdx == -1 || ready.Status != corev1.ConditionTrue {
		return false
	}
	if _, err := kl.patchNodeStatus(originalNode, node); err != nil {
		klog.ErrorS(err, "Error updating node status, will retry with syncNodeStatus")
		kl.syncNodeStatusMux.Unlock()
		kl.syncNodeStatus()
		kl.syncNodeStatusMux.Lock()
	}
	return true
}

func (kl *Cubelet) syncNodeStatus() {
	kl.syncNodeStatusMux.Lock()
	defer kl.syncNodeStatusMux.Unlock()
	ctx := context.Background()
	if kl.masterClient == nil {
		return
	}
	if kl.registerNode {
		kl.registerWithAPIServer()
	}
	if err := kl.updateNodeStatus(ctx); err != nil {
		klog.ErrorS(err, "Unable to update node status")
	}
}

func (kl *Cubelet) updateNodeStatus(ctx context.Context) error {
	for i := 0; i < nodeStatusUpdateRetry; i++ {
		if err := kl.tryUpdateNodeStatus(ctx, i); err != nil {
			klog.ErrorS(err, "Error updating node status, will retry")
			continue
		}
		return nil
	}
	return fmt.Errorf("update node status exceeds retry count")
}

func (kl *Cubelet) tryUpdateNodeStatus(ctx context.Context, tryNumber int) error {
	originalNode, err := kl.GetNode()
	if err != nil {
		return fmt.Errorf("error getting node %q: %v", kl.nodeName, err)
	}
	node, changed := kl.updateNode(ctx, originalNode)
	now := kl.clock.Now()
	if changed || kl.lastStatusReportTime.IsZero() {
		kl.delayAfterNodeStatusChange = kl.calculateDelay()
	} else {
		kl.delayAfterNodeStatusChange = 0
	}
	if !kl.shouldPatchNodeStatus(changed, tryNumber, now) {
		return nil
	}
	_, err = kl.patchNodeStatus(originalNode, node)
	return err
}

func (kl *Cubelet) shouldPatchNodeStatus(changed bool, tryNumber int, now time.Time) bool {
	if changed || kl.lastStatusReportTime.IsZero() || tryNumber > 0 {
		return true
	}
	if kl.nodeStatusReportFrequency <= 0 {
		return true
	}
	return !now.Before(kl.lastStatusReportTime.Add(kl.nodeStatusReportFrequency))
}

func (kl *Cubelet) updateDefaultLabels(initialNode, existingNode *cubeletnodemeta.Node) bool {
	defaultLabels := []string{
		corev1.LabelHostname,
		corev1.LabelTopologyZone,
		corev1.LabelTopologyRegion,
		corev1.LabelFailureDomainBetaZone,
		corev1.LabelFailureDomainBetaRegion,
		corev1.LabelInstanceTypeStable,
		corev1.LabelInstanceType,
		corev1.LabelOSStable,
		corev1.LabelArchStable,
		corev1.LabelWindowsBuild,
		kubeletapis.LabelOS,
		kubeletapis.LabelArch,
	}

	needsUpdate := false
	if existingNode.Labels == nil {
		existingNode.Labels = make(map[string]string)
	}
	for _, label := range defaultLabels {
		if _, hasInitialValue := initialNode.Labels[label]; !hasInitialValue {
			continue
		}
		if existingNode.Labels[label] != initialNode.Labels[label] {
			existingNode.Labels[label] = initialNode.Labels[label]
			needsUpdate = true
		}
		if existingNode.Labels[label] == "" {
			delete(existingNode.Labels, label)
		}
	}
	return needsUpdate
}

func updateDefaultResources(initialNode, existingNode *cubeletnodemeta.Node) bool {
	requiresUpdate := false
	if existingNode.Status.Capacity == nil {
		existingNode.Status.Capacity = map[corev1.ResourceName]resource.Quantity{}
	}
	if existingNode.Status.Allocatable == nil {
		existingNode.Status.Allocatable = map[corev1.ResourceName]resource.Quantity{}
	}
	if len(existingNode.Status.Capacity) == 0 && len(initialNode.Status.Capacity) > 0 {
		existingNode.Status.Capacity = cloneResourceList(initialNode.Status.Capacity)
		requiresUpdate = true
	}
	if len(existingNode.Status.Allocatable) == 0 && len(initialNode.Status.Allocatable) > 0 {
		existingNode.Status.Allocatable = cloneResourceList(initialNode.Status.Allocatable)
		requiresUpdate = true
	}
	return requiresUpdate
}

func (kl *Cubelet) updateNode(ctx context.Context, originalNode *cubeletnodemeta.Node) (*cubeletnodemeta.Node, bool) {
	node := originalNode.DeepCopy()
	areRequiredLabelsNotPresent := false
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	if node.Labels[corev1.LabelOSStable] != goruntime.GOOS {
		node.Labels[corev1.LabelOSStable] = goruntime.GOOS
		areRequiredLabelsNotPresent = true
	}
	if node.Labels[corev1.LabelArchStable] != goruntime.GOARCH {
		node.Labels[corev1.LabelArchStable] = goruntime.GOARCH
		areRequiredLabelsNotPresent = true
	}
	kl.setNodeStatus(ctx, node)
	changed := nodeStatusHasChanged(&originalNode.Status, &node.Status) || areRequiredLabelsNotPresent
	return node, changed
}

func (kl *Cubelet) patchNodeStatus(originalNode, node *cubeletnodemeta.Node) (*cubeletnodemeta.Node, error) {
	if kl.masterClient != nil {
		if err := kl.masterClient.UpdateNodeStatus(context.TODO(), string(kl.nodeName), kl.buildStatusRequest(node)); err != nil {
			return nil, err
		}
	}
	kl.lastStatusReportTime = kl.clock.Now()
	kl.lastNodeSnapshot = node.DeepCopy()
	return kl.lastNodeSnapshot.DeepCopy(), nil
}

func (kl *Cubelet) calculateDelay() time.Duration {
	return time.Duration(float64(kl.nodeStatusReportFrequency) * (-0.5 + rand.Float64()))
}

func nodeStatusHasChanged(originalStatus *cubeletnodemeta.NodeStatus, status *cubeletnodemeta.NodeStatus) bool {
	if originalStatus == nil && status == nil {
		return false
	}
	if originalStatus == nil || status == nil {
		return true
	}
	if nodeConditionsHaveChanged(originalStatus.Conditions, status.Conditions) {
		return true
	}
	originalCopy := originalStatus.DeepCopy()
	statusCopy := status.DeepCopy()
	originalCopy.Conditions = nil
	statusCopy.Conditions = nil
	if !apiequality.Semantic.DeepEqual(originalCopy.CubeImages, statusCopy.CubeImages) {
		klog.Info("CubeImages have changed")
	}
	return !apiequality.Semantic.DeepEqual(originalCopy, statusCopy)
}

func nodeConditionsHaveChanged(originalConditions []corev1.NodeCondition, conditions []corev1.NodeCondition) bool {
	if len(originalConditions) != len(conditions) {
		return true
	}
	sort.Slice(originalConditions, func(i, j int) bool { return originalConditions[i].Type < originalConditions[j].Type })
	sort.Slice(conditions, func(i, j int) bool { return conditions[i].Type < conditions[j].Type })
	for i := range conditions {
		if originalConditions[i].Type != conditions[i].Type ||
			originalConditions[i].Status != conditions[i].Status ||
			originalConditions[i].Reason != conditions[i].Reason ||
			originalConditions[i].Message != conditions[i].Message {
			return true
		}
	}
	return false
}

func ReadyCondition(
	nowFunc func() time.Time,
	runtimeErrorsFunc func() error,
	networkErrorsFunc func() error,
	storageErrorsFunc func() error,
	nodeShutdownManagerErrorsFunc func() error,
	recordEventFunc func(eventType, event string),
	localStorageCapacityIsolation bool,
) nodestatus.Setter {
	_ = recordEventFunc
	return func(ctx context.Context, node *cubeletnodemeta.Node) error {
		currentTime := metav1.NewTime(nowFunc())
		newNodeReadyCondition := corev1.NodeCondition{
			Type:              corev1.NodeReady,
			Status:            corev1.ConditionTrue,
			Reason:            "CubeletReady",
			Message:           "Cubelet is posting ready status",
			LastHeartbeatTime: currentTime,
		}
		errs := []error{runtimeErrorsFunc(), networkErrorsFunc(), storageErrorsFunc(), nodeShutdownManagerErrorsFunc()}
		requiredCapacities := []corev1.ResourceName{}
		if localStorageCapacityIsolation {
			requiredCapacities = append(requiredCapacities, corev1.ResourceEphemeralStorage)
		}
		missingCapacities := []string{}
		for _, resourceName := range requiredCapacities {
			if _, found := node.Status.Capacity[resourceName]; !found {
				missingCapacities = append(missingCapacities, string(resourceName))
			}
		}
		if len(missingCapacities) > 0 {
			errs = append(errs, fmt.Errorf("missing node capacity for resources: %s", strings.Join(missingCapacities, ", ")))
		}
		if aggregatedErr := utilerrors.NewAggregate(errs); aggregatedErr != nil {
			newNodeReadyCondition = corev1.NodeCondition{
				Type:              corev1.NodeReady,
				Status:            corev1.ConditionFalse,
				Reason:            "cubeletNotReady",
				Message:           aggregatedErr.Error(),
				LastHeartbeatTime: currentTime,
			}
		}
		readyConditionUpdated := false
		for i := range node.Status.Conditions {
			if node.Status.Conditions[i].Type == corev1.NodeReady {
				if node.Status.Conditions[i].Status == newNodeReadyCondition.Status {
					newNodeReadyCondition.LastTransitionTime = node.Status.Conditions[i].LastTransitionTime
				} else {
					newNodeReadyCondition.LastTransitionTime = currentTime
				}
				node.Status.Conditions[i] = newNodeReadyCondition
				readyConditionUpdated = true
				break
			}
		}
		if !readyConditionUpdated {
			newNodeReadyCondition.LastTransitionTime = currentTime
			node.Status.Conditions = append(node.Status.Conditions, newNodeReadyCondition)
		}
		return nil
	}
}

func (kl *Cubelet) buildRegisterRequest(node *cubeletnodemeta.Node) *masterclient.RegisterNodeRequest {
	req := &masterclient.RegisterNodeRequest{
		NodeID:       node.Name,
		HostIP:       firstNodeIP(node.Status.Addresses),
		Labels:       cloneLabels(node.Labels),
		Capacity:     toResourceSnapshot(node.Status.Capacity),
		Allocatable:  toResourceSnapshot(node.Status.Allocatable),
		InstanceType: kl.instanceType,
	}
	var hostCfg *config.HostConf
	if cfg := config.GetConfig(); cfg != nil {
		hostCfg = cfg.HostConf
	}
	if hostCfg != nil {
		req.ClusterLabel = hostCfg.SchedulerLabel
		req.CreateConcurrentNum = int64(hostCfg.Quota.CreationConcurrentNum)
	}
	req.QuotaCPU = resolveHostQuotaCPUMilli(hostCfg, req.Allocatable.MilliCPU, req.Capacity.MilliCPU)
	req.QuotaMemMB = resolveHostQuotaMemMB(hostCfg, req.Allocatable.MemoryMB, req.Capacity.MemoryMB)
	req.MaxMvmNum = resolveHostMaxMvmNum(hostCfg, req.QuotaMemMB)
	versions, incomplete := kl.collectVersionReport()
	req.Versions = versions
	req.InventoryIncomplete = incomplete
	return req
}

func (kl *Cubelet) buildStatusRequest(node *cubeletnodemeta.Node) *masterclient.UpdateNodeStatusRequest {
	req := &masterclient.UpdateNodeStatusRequest{
		Conditions:     append([]corev1.NodeCondition(nil), node.Status.Conditions...),
		Images:         append([]cubeletnodemeta.ContainerImage(nil), node.Status.CubeImages...),
		LocalTemplates: append([]cubeletnodemeta.LocalTemplate(nil), node.Status.CubeTemplates...),
		HeartbeatTime:  kl.clock.Now(),
	}
	attachResourceReport(req, kl.clock.Now())
	versions, incomplete := kl.collectVersionReport()
	req.Versions = versions
	req.InventoryIncomplete = incomplete
	return req
}

func (kl *Cubelet) collectVersionReport() ([]masterclient.ComponentVersion, bool) {
	if kl.versionCollector == nil {
		return nil, false
	}
	report := kl.versionCollector.CollectReport()
	if len(report.Versions) == 0 {
		return nil, report.Incomplete
	}
	out := make([]masterclient.ComponentVersion, 0, len(report.Versions))
	for _, v := range report.Versions {
		out = append(out, masterclient.ComponentVersion{
			Component: v.Component,
			Version:   v.Version,
			Commit:    v.Commit,
			BuildTime: v.BuildTime,
			Source:    v.Source,
			Variant:   v.Variant,
		})
	}
	return out, report.Incomplete
}

// attachResourceReport folds the allocated-resource and disk-usage views
// onto an outgoing status request. The lookup is lazy so the cubelet does
// not need to know whether the cubebox service plugin has finished
// initialising; missing data just skips the field. MetricTime stays
// zero-valued when nothing was attached, which cubemaster treats as
// "no metric in this heartbeat".
func attachResourceReport(req *masterclient.UpdateNodeStatusRequest, now time.Time) {
	collector := resourcesource.Get()
	if collector == nil {
		return
	}
	if alloc := collector.CollectAllocated(); alloc != nil {
		req.Allocated = &masterclient.AllocatedResources{
			MilliCPU:      alloc.MilliCPU,
			MemoryMB:      alloc.MemoryMB,
			MvmNum:        alloc.MvmNum,
			MvmRunningNum: alloc.MvmRunningNum,
			NicQueues:     alloc.NicQueues,
			DataDiskMB:    alloc.DataDiskMB,
			StorageDiskMB: alloc.StorageDiskMB,
		}
		req.MetricTime = now
	}
	if du := collector.CollectDiskUsage(); du != nil {
		req.DiskUsage = &masterclient.DiskUsage{
			DataDiskUsagePer:    du.DataDiskUsagePer,
			StorageDiskUsagePer: du.StorageDiskUsagePer,
			SysDiskUsagePer:     du.SysDiskUsagePer,
		}
		if req.MetricTime.IsZero() {
			req.MetricTime = now
		}
	}
}

func applyHostQuota(node *cubeletnodemeta.Node) {
	var hostCfg *config.HostConf
	if cfg := config.GetConfig(); cfg != nil {
		hostCfg = cfg.HostConf
	}
	applyHostQuotaWithConfig(node, hostCfg)
}

func applyHostQuotaWithConfig(node *cubeletnodemeta.Node, hostCfg *config.HostConf) {
	if node == nil || hostCfg == nil {
		return
	}
	if node.Status.Capacity == nil {
		node.Status.Capacity = map[corev1.ResourceName]resource.Quantity{}
	}
	if node.Status.Allocatable == nil {
		node.Status.Allocatable = map[corev1.ResourceName]resource.Quantity{}
	}
	if cpuMilli := resolveHostQuotaCPUMilli(hostCfg, 0, 0); cpuMilli > 0 {
		q := *resource.NewMilliQuantity(cpuMilli, resource.DecimalSI)
		node.Status.Capacity[corev1.ResourceCPU] = q.DeepCopy()
		node.Status.Allocatable[corev1.ResourceCPU] = q.DeepCopy()
	}
	if memMB := resolveHostQuotaMemMB(hostCfg, 0, 0); memMB > 0 {
		q := *resource.NewQuantity(memMB*1024*1024, resource.BinarySI)
		node.Status.Capacity[corev1.ResourceMemory] = q.DeepCopy()
		node.Status.Allocatable[corev1.ResourceMemory] = q.DeepCopy()
	}
}

func resolveHostQuotaCPUMilli(hostCfg *config.HostConf, fallbacks ...int64) int64 {
	if hostCfg != nil && hostCfg.Quota.Cpu > 0 {
		return int64(hostCfg.Quota.Cpu)
	}
	for _, fallback := range fallbacks {
		if fallback > 0 {
			return fallback
		}
	}
	cpuCount := hostCPUCount()
	if cpuCount <= 0 {
		return 0
	}
	return int64(cpuCount) * 1000 * defaultHostQuotaCPUOvercommitFactor
}

func resolveHostQuotaMemMB(hostCfg *config.HostConf, fallbacks ...int64) int64 {
	if hostCfg != nil {
		if memMB := parseMemMB(hostCfg.Quota.Mem); memMB > 0 {
			return memMB
		}
	}
	for _, fallback := range fallbacks {
		if fallback > 0 {
			return fallback
		}
	}
	memMB, err := readHostMemoryTotalMB()
	if err != nil {
		klog.ErrorS(err, "Failed to detect host memory for default quota")
		return 0
	}
	if memMB <= 0 {
		return 0
	}
	return roundUpFraction(memMB, defaultHostQuotaMemNumerator, defaultHostQuotaMemDenominator)
}

func resolveHostMaxMvmNum(hostCfg *config.HostConf, quotaMemMB int64) int64 {
	if hostCfg != nil && hostCfg.Quota.MvmLimit > 0 {
		return int64(hostCfg.Quota.MvmLimit)
	}
	if quotaMemMB <= 0 {
		return 0
	}
	maxMVMNum := quotaMemMB / defaultHostMemPerMVMMB
	if maxMVMNum <= 0 {
		return 1
	}
	return maxMVMNum
}

func parseMemMB(value string) int64 {
	if value == "" {
		return 0
	}
	q, err := resource.ParseQuantity(value)
	if err != nil {
		return 0
	}
	return q.Value() / (1024 * 1024)
}

func toResourceSnapshot(in map[corev1.ResourceName]resource.Quantity) masterclient.ResourceSnapshot {
	out := masterclient.ResourceSnapshot{}
	if cpu, ok := in[corev1.ResourceCPU]; ok {
		out.MilliCPU = cpu.MilliValue()
	}
	if mem, ok := in[corev1.ResourceMemory]; ok {
		out.MemoryMB = mem.Value() / (1024 * 1024)
	}
	return out
}

func firstNodeIP(addresses []corev1.NodeAddress) string {
	for _, addr := range addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}
	}
	return ""
}

func cloneLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneResourceList(in map[corev1.ResourceName]resource.Quantity) map[corev1.ResourceName]resource.Quantity {
	out := make(map[corev1.ResourceName]resource.Quantity, len(in))
	for k, v := range in {
		out[k] = v.DeepCopy()
	}
	return out
}

func detectHostMemoryTotalMB() (int64, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("unexpected MemTotal format: %q", line)
		}
		memKB, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse MemTotal failed: %w", err)
		}
		return roundUpFraction(memKB, 1, 1024), nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}

func roundUpFraction(value int64, numerator int64, denominator int64) int64 {
	if value <= 0 || numerator <= 0 || denominator <= 0 {
		return 0
	}
	return (value*numerator + denominator - 1) / denominator
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubelet

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	cubeletnodemeta "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet/nodemeta"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/masterclient"
)

type MockCubeMetaController struct {
	runCalled bool
	runError  error
}

func (m *MockCubeMetaController) Run(stopCh <-chan struct{}) error {
	m.runCalled = true
	return m.runError
}

type MockRunTemplateManager struct {
	instanceType string
}

func (m *MockRunTemplateManager) SetInstanceType(instanceType string) {
	m.instanceType = instanceType
}

func (m *MockRunTemplateManager) IsReady() bool {
	return true
}

func (m *MockRunTemplateManager) EnsureCubeRunTemplate(ctx context.Context, templateID string) (*templatetypes.LocalRunTemplate, error) {
	return &templatetypes.LocalRunTemplate{
		DistributionReference: templatetypes.DistributionReference{
			TemplateID: templateID,
		},
	}, nil
}

func (m *MockRunTemplateManager) ListLocalTemplates(ctx context.Context) (map[string]*templatetypes.LocalRunTemplate, error) {
	return make(map[string]*templatetypes.LocalRunTemplate), nil
}

func (m *MockRunTemplateManager) EnsureLocalTemplate(ctx context.Context, templateID string) (*templatetypes.LocalRunTemplate, error) {
	return m.EnsureCubeRunTemplate(ctx, templateID)
}

func TestNewCubelet(t *testing.T) {
	tests := []struct {
		name       string
		config     *KubeletConfig
		restConfig *masterclient.Client
		shouldFail bool
	}{
		{
			name: "successful cubelet creation with default config",
			config: &KubeletConfig{
				Insecurity:        true,
				ResyncInterval:    10 * time.Hour,
				DisableCreateNode: false,
			},
			restConfig: nil,
			shouldFail: false,
		},
		{
			name:       "cubelet creation with nil config",
			config:     nil,
			restConfig: nil,
			shouldFail: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := tt.config
			if config == nil {
				config = DefaultCubeletConfig()
			}

			controllerMap := make(map[string]controller.CubeMetaController)
			mockRunTemplateManager := &MockRunTemplateManager{}

			clet, err := NewCubelet(
				config,
				tt.restConfig,
				controllerMap,
				nil,
				mockRunTemplateManager,
			)

			if (err != nil) != tt.shouldFail {
				t.Fatalf("NewCubelet() error = %v, shouldFail = %v", err, tt.shouldFail)
			}

			if !tt.shouldFail && clet == nil {
				t.Fatal("Expected Cubelet instance, got nil")
			}

			if !tt.shouldFail && clet != nil {

				if clet.hostname == "" {
					t.Error("hostname should not be empty")
				}

				if clet.nodeName == "" {
					t.Error("nodeName should not be empty")
				}

				if len(clet.nodeIPs) == 0 {
					t.Error("nodeIPs should not be empty")
				}

				if clet.NodeRef == nil {
					t.Error("nodeRef should be initialized")
				}

				if clet.NodeRef.Kind != "Node" {
					t.Errorf("nodeRef.Kind should be 'Node', got %s", clet.NodeRef.Kind)
				}

				if clet.clock == nil {
					t.Error("clock should be initialized")
				}

				t.Logf("✓ Cubelet created successfully: hostname=%s, nodeName=%s", clet.hostname, clet.nodeName)
			}
		})
	}
}

func TestCubeletRun(t *testing.T) {
	config := &KubeletConfig{
		Insecurity:        true,
		ResyncInterval:    10 * time.Hour,
		DisableCreateNode: false,
	}

	controllerMap := make(map[string]controller.CubeMetaController)
	mockController := &MockCubeMetaController{}
	controllerMap["mock-controller"] = mockController

	mockRunTemplateManager := &MockRunTemplateManager{}

	clet, err := NewCubelet(
		config,
		nil,
		controllerMap,
		nil,
		mockRunTemplateManager,
	)
	if err != nil {
		t.Fatalf("Failed to create Cubelet: %v", err)
	}

	if clet == nil {
		t.Fatal("Expected Cubelet instance, got nil")
	}

	t.Logf("Created Cubelet in standalone mode: %s", clet.hostname)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- clet.Run(func() {})
	}()

	select {
	case err := <-errCh:
		if err != nil && err != context.DeadlineExceeded {
			t.Fatalf("Cubelet.Run() returned error: %v", err)
		}
		t.Log("✓ Cubelet.Run() completed successfully")

	case <-ctx.Done():
		t.Log("✓ Cubelet.Run() context timeout (expected behavior for background processes)")
	}

	if !mockController.runCalled {
		t.Error("Expected MockCubeMetaController.Run() to be called")
	}

	t.Log("✓ Cubelet startup test passed")
}

func TestCubeletNodeStatusInitialization(t *testing.T) {
	config := DefaultCubeletConfig()

	controllerMap := make(map[string]controller.CubeMetaController)
	mockRunTemplateManager := &MockRunTemplateManager{}

	clet, err := NewCubelet(
		config,
		nil,
		controllerMap,
		nil,
		mockRunTemplateManager,
	)
	if err != nil {
		t.Fatalf("Failed to create Cubelet: %v", err)
	}

	if len(clet.SetNodeStatusFuncs) == 0 {
		t.Error("setNodeStatusFuncs should not be empty")
	}

	t.Logf("✓ Node status functions initialized: %d functions", len(clet.SetNodeStatusFuncs))

	expectedLabels := []string{
		corev1.LabelHostname,
		corev1.LabelMetadataName,
		constants.LabelInstanceType,
	}

	for _, label := range expectedLabels {
		if _, exists := clet.NodeLabels[label]; !exists {
			t.Errorf("Expected node label %s not found", label)
		}
	}

	t.Logf("✓ Node labels initialized correctly: %d labels", len(clet.NodeLabels))
}

func TestCubeletIntegrationWithMockControllers(t *testing.T) {
	config := &KubeletConfig{
		Insecurity:        true,
		ResyncInterval:    10 * time.Hour,
		DisableCreateNode: false,
	}

	controllerMap := make(map[string]controller.CubeMetaController)
	controllers := map[string]*MockCubeMetaController{
		"controller-1": {},
		"controller-2": {},
		"controller-3": {},
	}

	for name, ctrl := range controllers {
		controllerMap[name] = ctrl
	}

	mockRunTemplateManager := &MockRunTemplateManager{}

	clet, err := NewCubelet(
		config,
		nil,
		controllerMap,
		nil,
		mockRunTemplateManager,
	)
	if err != nil {
		t.Fatalf("Failed to create Cubelet: %v", err)
	}

	if clet == nil {
		t.Fatal("Expected Cubelet instance, got nil")
	}

	if len(clet.controllerMap) != len(controllerMap) {
		t.Errorf("Expected %d controllers, got %d", len(controllerMap), len(clet.controllerMap))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- clet.Run(func() {})
	}()

	select {
	case err := <-errCh:
		if err != nil && err != context.DeadlineExceeded {
			t.Fatalf("Cubelet.Run() failed: %v", err)
		}
	case <-ctx.Done():

	}

	for name, ctrl := range controllers {
		if !ctrl.runCalled {
			t.Errorf("Expected controller %s to be called", name)
		}
	}

	t.Logf("✓ All %d controllers were executed successfully", len(controllerMap))
}

func TestApplyHostQuotaWithConfigUsesMachineDefaults(t *testing.T) {
	oldCPUCount := hostCPUCount
	oldReadHostMemoryTotalMB := readHostMemoryTotalMB
	t.Cleanup(func() {
		hostCPUCount = oldCPUCount
		readHostMemoryTotalMB = oldReadHostMemoryTotalMB
	})

	hostCPUCount = func() int { return 2 }
	readHostMemoryTotalMB = func() (int64, error) { return 8192, nil }

	node := &cubeletnodemeta.Node{
		Status: cubeletnodemeta.NodeStatus{
			Capacity:    map[corev1.ResourceName]resource.Quantity{},
			Allocatable: map[corev1.ResourceName]resource.Quantity{},
		},
	}
	applyHostQuotaWithConfig(node, &config.HostConf{})

	cpuQuantity := node.Status.Capacity[corev1.ResourceCPU]
	memQuantity := node.Status.Capacity[corev1.ResourceMemory]
	cpu := cpuQuantity.MilliValue()
	memMB := memQuantity.Value() / (1024 * 1024)
	if cpu != 4000 {
		t.Fatalf("expected default cpu quota 4000m, got %dm", cpu)
	}
	if memMB != 10240 {
		t.Fatalf("expected default mem quota 10240Mi, got %dMi", memMB)
	}
}

func TestApplyHostQuotaWithConfigPrefersConfiguredQuota(t *testing.T) {
	oldCPUCount := hostCPUCount
	oldReadHostMemoryTotalMB := readHostMemoryTotalMB
	t.Cleanup(func() {
		hostCPUCount = oldCPUCount
		readHostMemoryTotalMB = oldReadHostMemoryTotalMB
	})

	hostCPUCount = func() int { return 64 }
	readHostMemoryTotalMB = func() (int64, error) { return 262144, nil }

	node := &cubeletnodemeta.Node{
		Status: cubeletnodemeta.NodeStatus{
			Capacity:    map[corev1.ResourceName]resource.Quantity{},
			Allocatable: map[corev1.ResourceName]resource.Quantity{},
		},
	}
	applyHostQuotaWithConfig(node, &config.HostConf{
		Quota: config.HostConfigQuota{
			Cpu: 3000,
			Mem: "6Gi",
		},
	})

	cpuQuantity := node.Status.Capacity[corev1.ResourceCPU]
	memQuantity := node.Status.Capacity[corev1.ResourceMemory]
	cpu := cpuQuantity.MilliValue()
	memMB := memQuantity.Value() / (1024 * 1024)
	if cpu != 3000 {
		t.Fatalf("expected configured cpu quota 3000m, got %dm", cpu)
	}
	if memMB != 6144 {
		t.Fatalf("expected configured mem quota 6144Mi, got %dMi", memMB)
	}
}

func TestResolveHostMaxMvmNumUsesMemBasedDefault(t *testing.T) {
	maxMVMNum := resolveHostMaxMvmNum(&config.HostConf{}, 9450)
	if maxMVMNum != 18 {
		t.Fatalf("expected default max mvm num 18, got %d", maxMVMNum)
	}
}

func TestResolveHostMaxMvmNumPrefersConfiguredLimit(t *testing.T) {
	maxMVMNum := resolveHostMaxMvmNum(&config.HostConf{
		Quota: config.HostConfigQuota{
			MvmLimit: 123,
		},
	}, 9450)
	if maxMVMNum != 123 {
		t.Fatalf("expected configured max mvm num 123, got %d", maxMVMNum)
	}
}

func TestCubeletNodeInformation(t *testing.T) {
	config := DefaultCubeletConfig()

	controllerMap := make(map[string]controller.CubeMetaController)
	mockRunTemplateManager := &MockRunTemplateManager{}

	clet, err := NewCubelet(
		config,
		nil,
		controllerMap,
		nil,
		mockRunTemplateManager,
	)
	if err != nil {
		t.Fatalf("Failed to create Cubelet: %v", err)
	}

	if clet.hostname == "" {
		t.Error("Hostname should not be empty")
	}
	t.Logf("Hostname: %s", clet.hostname)

	if len(clet.nodeIPs) == 0 {
		t.Error("Node IPs should not be empty")
	}

	for i, ip := range clet.nodeIPs {
		if ip == nil {
			t.Errorf("Node IP %d is nil", i)
		} else {
			t.Logf("Node IP %d: %s", i, ip.String())
		}
	}

	if clet.instanceType != "cubebox" {
		t.Errorf("Instance type should be cubebox, got %s", clet.instanceType)
	}
	t.Logf("Instance type: %s", clet.instanceType)

	if clet.providerID == "" {
		t.Error("Provider ID should not be empty")
	}
	t.Logf("Provider ID: %s", clet.providerID)

	if clet.NodeRef == nil {
		t.Fatal("Node reference should not be nil")
	}

	if clet.NodeRef.Kind != "Node" {
		t.Errorf("Node reference kind should be 'Node', got '%s'", clet.NodeRef.Kind)
	}

	if clet.NodeRef.Name != clet.hostname {
		t.Errorf("Node reference name should be '%s', got '%s'", clet.hostname, clet.NodeRef.Name)
	}

	t.Log("✓ All node information initialized correctly")
}

func TestCubeletStandaloneMode(t *testing.T) {
	config := &KubeletConfig{
		Insecurity:        true,
		ResyncInterval:    10 * time.Hour,
		DisableCreateNode: true,
	}

	controllerMap := make(map[string]controller.CubeMetaController)
	mockRunTemplateManager := &MockRunTemplateManager{}

	clet, err := NewCubelet(
		config,
		nil,
		controllerMap,
		nil,
		mockRunTemplateManager,
	)
	if err != nil {
		t.Fatalf("Failed to create Cubelet: %v", err)
	}

	if clet.kubeClient != nil {
		t.Error("kubeClient should be nil in standalone mode")
	}

	if clet.nodeLister == nil {
		t.Error("nodeLister should be initialized even in standalone mode")
	}

	if !clet.NodeHasSynced() {
		t.Error("nodeHasSynced should return true in standalone mode")
	}

	t.Log("✓ Standalone mode configuration is correct")
}

func TestNetworkErrorsFunc(t *testing.T) {
	cubelet := &Cubelet{}
	if err := cubelet.networkErrorsFunc(); err != nil {
		t.Fatalf("networkErrorsFunc() unexpected error: %v", err)
	}
}

func BenchmarkNewCubelet(b *testing.B) {
	config := DefaultCubeletConfig()
	controllerMap := make(map[string]controller.CubeMetaController)
	mockRunTemplateManager := &MockRunTemplateManager{}

	b.ResetTimer()
	for range b.N {
		_, err := NewCubelet(
			config,
			nil,
			controllerMap,
			nil,
			mockRunTemplateManager,
		)
		if err != nil {
			b.Fatalf("Failed to create Cubelet: %v", err)
		}
	}
}

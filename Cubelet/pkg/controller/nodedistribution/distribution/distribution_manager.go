// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package distribution

import (
	"context"
	"fmt"
	"sync"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

type TaskSource string

const (
	TaskSourceDistribution TaskSource = "Distribution"
	TaskSourceRequest      TaskSource = "Request"
)

const (
	defaultCurrency = int32(100)
)

type ResourceTaskType string

type TaskStatusCode string

const (
	TaskStatus_PENDING  TaskStatusCode = "PENDING"
	TaskStatus_RUNNING  TaskStatusCode = "RUNNING"
	TaskStatus_SUCCESS  TaskStatusCode = "SUCCESS"
	TaskStatus_FAILED   TaskStatusCode = "FAILED"
	TaskStatus_CANCELED TaskStatusCode = "CANCELED"
)

type DistributionTask struct {
	Id          string            `json:"id,omitempty"`
	Name        string            `json:"name,omitempty"`
	Type        string            `json:"type,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

const (
	ResourceTaskTypeCubeRunTemplate ResourceTaskType = "CubeRunTemplate"
	ResourceTaskTypeSnapshot        ResourceTaskType = "Snapshot"
	ResourceTaskTypeImage           ResourceTaskType = "Image"
	ResourceTaskTypeComponent       ResourceTaskType = "Component"
	ResourceTaskTypeBaseBlockVolume ResourceTaskType = "BaseBlockVolume"
)

type TaskHandler interface {
	Handle(ctx context.Context, task *SubTaskDefine) (TaskStatus, error)

	IsReady() bool
}

type DistributionTaskManager interface {
	ProcessTask(ctx context.Context, task DistributionTask) (status TaskStatus, err error)

	GetTaskStatus(ctx context.Context, taskID string) (TaskStatus, error)

	SetDistributionConcurrency(ctx context.Context, distributionID string, currency int32) error
}

type distributionConcurrency struct {
	semaphore chan struct{}
	currency  int32
}

type DefaultDistributionManager struct {
	taskStatusMutex sync.RWMutex
	taskStatus      map[string]TaskStatus

	distributionConcurrencyMutex sync.RWMutex
	distributionConcurrency      map[string]*distributionConcurrency

	defaultConcurrency *distributionConcurrency
}

var _ DistributionTaskManager = &DefaultDistributionManager{}

func NewDefaultDistributionManager() *DefaultDistributionManager {
	return &DefaultDistributionManager{
		taskStatus:              make(map[string]TaskStatus),
		distributionConcurrency: make(map[string]*distributionConcurrency),
		defaultConcurrency: &distributionConcurrency{
			semaphore: make(chan struct{}, defaultCurrency),
			currency:  defaultCurrency,
		},
	}
}

func (m *DefaultDistributionManager) SaveTaskStatus(ctx context.Context, status TaskStatus) error {
	m.taskStatusMutex.Lock()
	defer m.taskStatusMutex.Unlock()

	if base, ok := status.(*BaseSubTaskStatus); ok {
		m.taskStatus[base.DistributionTaskID] = status

		log.G(ctx).Infof("Saved task status: taskID=%s, status=%s, retryCount=%d", base.DistributionTaskID, base.Status, base.RetryCount)
	}

	return nil
}

func (m *DefaultDistributionManager) GetTaskStatus(ctx context.Context, taskID string) (TaskStatus, error) {
	m.taskStatusMutex.RLock()
	defer m.taskStatusMutex.RUnlock()

	if taskStatus, exists := m.taskStatus[taskID]; exists {
		return taskStatus, nil
	}

	return nil, nil
}

func (m *DefaultDistributionManager) SetDistributionConcurrency(ctx context.Context, distributionID string, currency int32) error {
	if currency <= 0 {
		currency = 1
	}

	m.distributionConcurrencyMutex.Lock()
	defer m.distributionConcurrencyMutex.Unlock()

	if distributionID == "" {
		m.defaultConcurrency = &distributionConcurrency{
			semaphore: make(chan struct{}, currency),
			currency:  currency,
		}
		log.G(ctx).Infof("Set default concurrency level to %d", currency)
		return nil
	}

	m.distributionConcurrency[distributionID] = &distributionConcurrency{
		semaphore: make(chan struct{}, currency),
		currency:  currency,
	}
	log.G(ctx).Infof("Set concurrency level for distribution %s to %d", distributionID, currency)

	return nil
}

func (m *DefaultDistributionManager) GetDistributionConcurrency(ctx context.Context, distributionID string) (int32, error) {
	m.distributionConcurrencyMutex.RLock()
	defer m.distributionConcurrencyMutex.RUnlock()

	if distributionID == "" {
		return m.defaultConcurrency.currency, nil
	}

	if dc, exists := m.distributionConcurrency[distributionID]; exists {
		return dc.currency, nil
	}

	return m.defaultConcurrency.currency, nil
}

func (m *DefaultDistributionManager) getSemaphore(distributionID string) chan struct{} {
	m.distributionConcurrencyMutex.RLock()
	defer m.distributionConcurrencyMutex.RUnlock()

	if distributionID == "" {
		return m.defaultConcurrency.semaphore
	}

	if dc, exists := m.distributionConcurrency[distributionID]; exists {
		return dc.semaphore
	}

	return m.defaultConcurrency.semaphore
}

func (m *DefaultDistributionManager) ProcessTask(ctx context.Context, originTask DistributionTask) (status TaskStatus, err error) {
	taskID := originTask.Id

	log.G(ctx).Infof("Processing task %s", taskID)

	task := NewSubTaskDefine(ctx, taskID, originTask.Name, ResourceTaskType(originTask.Type))
	task.Annotations = originTask.Annotations
	task.Object = originTask

	distributionID := task.DistributionName
	semaphore := m.getSemaphore(distributionID)

	select {
	case semaphore <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-semaphore }()

	status = task.NewRunningStatus()
	var (
		subTaskStatus TaskStatus
		handler       TaskHandler
		baseStatus    = status.(*BaseSubTaskStatus)
	)
	oldStatus, err := m.GetTaskStatus(ctx, taskID)
	if err == nil {
		if base, ok := oldStatus.(*BaseSubTaskStatus); ok {
			baseStatus.RetryCount = base.RetryCount + 1
		}
	}

	defer func() {
		if subTaskStatus != nil {
			baseStatus.AddSubTaskStatus(ctx, subTaskStatus)
		}
		if err != nil {
			baseStatus.AddError(ctx, err)
		}
		m.SaveTaskStatus(ctx, status)
	}()

	handler, err = getTaskHandler(ResourceTaskType(task.Type))
	if err != nil {
		return
	}

	subTaskStatus, err = handler.Handle(ctx, task)
	if err != nil {
		return
	}

	return
}

var globalHandlerRegistry *TaskHandlerRegistry

func InitializeHandlerRegistry() {
	globalHandlerRegistry = &TaskHandlerRegistry{
		handlers: make(map[ResourceTaskType]TaskHandler),
	}
}

func InvokeTask(ctx context.Context, task *SubTaskDefine) (status TaskStatus, err error) {
	var (
		subStatus = task.NewRunningStatus()
		handle    TaskHandler
	)
	handle, err = getTaskHandler(task.Type)
	if err != nil {
		subStatus.AddError(ctx, err)
		return
	}
	return handle.Handle(ctx, task)
}

type TaskHandlerRegistry struct {
	handlers map[ResourceTaskType]TaskHandler
}

func getTaskHandler(taskType ResourceTaskType) (TaskHandler, error) {
	if globalHandlerRegistry == nil {
		return nil, fmt.Errorf("[%s] handler registry not initialized", taskType)
	}
	handler, exists := globalHandlerRegistry.handlers[taskType]
	if !exists {
		return nil, fmt.Errorf("no handler registered for task type: %v", taskType)
	}
	return handler, nil
}

func RegisterHandler(taskType ResourceTaskType, handler TaskHandler) {
	globalHandlerRegistry.handlers[taskType] = handler
}

type distributionNameKeyType struct{}

func WithDistributionName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, distributionNameKeyType{}, name)
}

func GetDistributionName(ctx context.Context) string {
	if name, ok := ctx.Value(distributionNameKeyType{}).(string); ok {
		return name
	}
	return ""
}

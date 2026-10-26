// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package distribution

import (
	"context"

	"github.com/containerd/containerd/v2/pkg/namespaces"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
)

func init() {
	InitializeHandlerRegistry()
}

type TaskStatus interface {
	AddError(ctx context.Context, err error)
	AddSubTaskStatus(ctx context.Context, subStatus TaskStatus) error
	GetStatus() TaskStatusCode
	SetStatus(status TaskStatusCode, msg string)
	GetType() ResourceTaskType
	GetMessage() string
	GetHumanableName() string
	GetRetryCount() int
	FinallySubState() TaskStatus

	GetExternObj() any
}

type TaskCommon struct {
	Name       string
	Namespace  string
	TaskSource TaskSource

	DistributionName   string
	DistributionTaskID string
	TemplateID         string
	Type               ResourceTaskType
	Annotations        map[string]string
}

func (task *TaskCommon) GetHumanableName() string {
	return string(task.Type) + "-" + task.Name
}

type SubTaskDefine struct {
	TaskCommon

	Object any
}

func NewSubTaskDefine(ctx context.Context, id, name string, Type ResourceTaskType) *SubTaskDefine {
	task := &SubTaskDefine{
		TaskCommon: TaskCommon{
			Name:               name,
			DistributionTaskID: id,
			Type:               Type,
		},
	}
	if v := GetDistributionName(ctx); v != "" {
		task.DistributionName = v
		task.TaskSource = TaskSourceDistribution
	} else {

		task.TaskSource = TaskSourceRequest
	}
	if Type == ResourceTaskTypeCubeRunTemplate {
		task.TemplateID = name
	}
	ns, _ := namespaces.NamespaceRequired(ctx)
	task.Namespace = ns
	return task
}

func (task *SubTaskDefine) NewRunningStatus() *BaseSubTaskStatus {
	return &BaseSubTaskStatus{
		TaskCommon: task.TaskCommon,
		Status:     TaskStatus_RUNNING,
	}
}

func (task *SubTaskDefine) NewSubTask() *SubTaskDefine {
	return &SubTaskDefine{
		TaskCommon: task.TaskCommon,
		Object:     task.Object,
	}
}

func (task *SubTaskDefine) GenDistributionReference() *templatetypes.DistributionReference {
	return &templatetypes.DistributionReference{
		Namespace:          task.Namespace,
		Name:               task.Name,
		DistributionName:   task.DistributionName,
		DistributionTaskID: task.DistributionTaskID,
		TemplateID:         task.TemplateID,
	}
}

func (task *SubTaskDefine) ToImageTask(image *templatetypes.TemplateImage) *SubTaskDefine {
	task.Type = ResourceTaskTypeImage
	task.Object = image
	return task
}

func (task *SubTaskDefine) ToVolumeTask(vs *templatetypes.LocalBaseVolume) *SubTaskDefine {
	task.Type = ResourceTaskTypeBaseBlockVolume
	task.Object = vs
	return task
}

func (task *SubTaskDefine) ToComponentTask(component templatetypes.MachineComponent) *SubTaskDefine {
	task.Type = ResourceTaskTypeSnapshot
	task.Object = component
	return task
}

func (task *SubTaskDefine) ToSnapshotTask(image templatetypes.Snapshot) *SubTaskDefine {
	task.Type = ResourceTaskTypeSnapshot
	task.Object = image
	return task
}

type BaseSubTaskStatus struct {
	TaskCommon
	Status     TaskStatusCode
	Message    string
	RetryCount int32

	SubTasks map[string]TaskStatus
}

func (status *BaseSubTaskStatus) AddError(ctx context.Context, err error) {
	status.Status = TaskStatus_FAILED
	status.Message = err.Error()
	log.G(ctx).WithField("taskID", status.DistributionTaskID).Error(status.Message)
}

func (status *BaseSubTaskStatus) AddSubTaskStatus(ctx context.Context, subStatus TaskStatus) error {
	if subStatus.GetStatus() == TaskStatus_FAILED {
		status.Status = TaskStatus_FAILED
		status.Message = subStatus.GetMessage()
	}
	if status.SubTasks == nil {
		status.SubTasks = make(map[string]TaskStatus)
	}
	status.SubTasks[subStatus.GetHumanableName()] = subStatus
	return nil
}

func (status *BaseSubTaskStatus) FinallySubState() TaskStatus {
	var (
		finalStatus  TaskStatusCode = status.Status
		finalMessage string         = status.Message
		successCount int            = 0
	)
	for _, subStatus := range status.SubTasks {
		if subStatus.GetStatus() == TaskStatus_FAILED {
			finalStatus = TaskStatus_FAILED
			finalMessage = subStatus.GetMessage()
			break
		}
		if subStatus.GetStatus() == TaskStatus_RUNNING {
			finalStatus = TaskStatus_RUNNING
			break
		}
		if subStatus.GetStatus() == TaskStatus_SUCCESS {
			successCount++
		}
	}
	if successCount == len(status.SubTasks) && successCount != 0 {
		finalStatus = TaskStatus_SUCCESS
		finalMessage = ""
	}
	status.Status = finalStatus
	status.Message = finalMessage
	return status
}

func (status *BaseSubTaskStatus) GetMessage() string {
	return status.Message
}

func (status *BaseSubTaskStatus) GetStatus() TaskStatusCode {
	return status.Status
}

func (status *BaseSubTaskStatus) SetStatus(s TaskStatusCode, msg string) {
	status.Status = s
	status.Message = msg
}

func (status *BaseSubTaskStatus) GetRetryCount() int {
	return int(status.RetryCount)
}

func (status *BaseSubTaskStatus) GetType() ResourceTaskType {
	return status.Type
}

func (status *BaseSubTaskStatus) GetExternObj() any {
	return status
}

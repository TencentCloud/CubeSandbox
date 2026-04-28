// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package shimapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/containerd/containerd/v2/client"
	"encoding/json"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/apis/shimapi/shimtypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

const (
	API_UPDATE_ACTION_KEY      = "cube.shimapi.update.action"
	API_UPDATE_ACTION_DATA_KEY = "cube.shimapi.update.data"
	API_ACTION_HOT_PLUGIN_ADD  = "HotplugDevice.Add"
	API_ACTION_HOT_PLUGIN_DEL  = "HotplugDevice.Del"
)

type CubeShimDeviceAPI interface {
	AddDevices(ctx context.Context, devices []*shimtypes.CubeShimDevice) error
	DelDevices(ctx context.Context, devices []*shimtypes.CubeShimDevice) error
}

func (csc *cubeShimControl) AddDevices(ctx context.Context, devices []*shimtypes.CubeShimDevice) error {
	sandbox := csc.cubebox

	var toAppendDevices []*shimtypes.CubeShimDevice
	if csc.cubebox.HotPlugDevices == nil {
		csc.cubebox.HotPlugDevices = make(map[string]*shimtypes.CubeShimDevice)
	}
	for i := range devices {
		if _, ok := csc.cubebox.HotPlugDevices[devices[i].ID]; !ok {
			toAppendDevices = append(toAppendDevices, devices[i])
		}
	}

	if len(toAppendDevices) > 0 {
		logEntry := log.G(ctx).WithFields(CubeLog.Fields{
			"cubebox":             sandbox.ID,
			"task":                csc.task.ID(),
			API_UPDATE_ACTION_KEY: API_ACTION_HOT_PLUGIN_ADD,
		})

		b, err := json.Marshal(toAppendDevices)
		if err != nil {
			logEntry.WithError(err).Errorf("failed to marshal device list")
			return fmt.Errorf("failed to marshal device list")
		}
		logEntry = logEntry.WithField(API_UPDATE_ACTION_DATA_KEY, string(b))
		if err := csc.task.Update(ctx, client.WithAnnotations(map[string]string{
			API_UPDATE_ACTION_KEY:      API_ACTION_HOT_PLUGIN_ADD,
			API_UPDATE_ACTION_DATA_KEY: b,
		})); err != nil {
			logEntry.WithError(err).Errorf("failed to update task for container %s", sandbox.FirstContainer().Container.ID())
			return fmt.Errorf("failed to update task for container %s", sandbox.FirstContainer().Container.ID())
		}
		logEntry.Infof("add device to task success")

		for i := range toAppendDevices {
			csc.cubebox.HotPlugDevices[toAppendDevices[i].ID] = toAppendDevices[i]
		}
	}
	return nil
}

func (csc *cubeShimControl) DelDevices(ctx context.Context, devices []*shimtypes.CubeShimDevice) error {
	sandbox := csc.cubebox

	var toADeletedDevices []*shimtypes.CubeShimDevice
	if csc.cubebox.HotPlugDevices == nil {
		csc.cubebox.HotPlugDevices = make(map[string]*shimtypes.CubeShimDevice)
	}
	for i := range devices {
		if _, ok := csc.cubebox.HotPlugDevices[devices[i].ID]; ok {
			delete(csc.cubebox.HotPlugDevices, devices[i].ID)
			toADeletedDevices = append(toADeletedDevices, devices[i])
		}
	}

	if len(toADeletedDevices) > 0 {
		logEntry := log.G(ctx).WithFields(CubeLog.Fields{
			"cubebox": sandbox.ID,
			"task":    csc.task.ID(),
			"action":  "HotPlugDevice.del",
		})

		b, err := json.Marshal(toADeletedDevices)
		if err != nil {
			logEntry.WithError(err).Errorf("failed to marshal device list")
			return fmt.Errorf("failed to marshal device list")
		}
		if err := csc.task.Update(ctx, client.WithAnnotations(map[string]string{
			API_UPDATE_ACTION_KEY:      API_ACTION_HOT_PLUGIN_DEL,
			API_UPDATE_ACTION_DATA_KEY: b,
		})); err != nil {
			logEntry.WithError(err).Errorf("failed to update task for container %s", sandbox.FirstContainer().Container.ID())
			return fmt.Errorf("failed to update task for container %s", sandbox.FirstContainer().Container.ID())
		}
		logEntry.WithField("devices", string(b)).Infof("del device to task")
	}
	return nil
}

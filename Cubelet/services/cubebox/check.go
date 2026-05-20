// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"io"
	"strings"
	"syscall"

	"github.com/containerd/fifo"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/taskio"
	"github.com/tencentcloud/CubeSandbox/cubelog"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
)

func checkParam(ctx context.Context, realReq *cubebox.RunCubeSandboxRequest) error {
	if err := checkReqVolumes(ctx, realReq); err != nil {
		return err
	}

	var err error
	nameSet := sets.NewString()
	for _, c := range realReq.Containers {
		if err = checkContainerName(c.Name, &nameSet); err != nil {
			break
		}
		if err = checkContainerVolumes(ctx, c); err != nil {
			break
		}

		if err = checkResource(ctx, c); err != nil {
			break
		}
		if err = checkProbe(ctx, c); err != nil {
			break
		}
	}

	for _, p := range realReq.GetExposedPorts() {
		if p <= 0 || p > 65535 {
			return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "invalid exposed port %d", p)
		}
	}
	if len(realReq.GetExposedPorts()) > 4 {
		return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat,
			"exposed ports should be at most 4")
	}

	if err != nil {
		return ret.Err(errorcode.ErrorCode_InvalidParamFormat, err.Error())
	} else {
		return nil
	}
}

func checkContainerName(name string, nameSet *sets.String) error {
	if name == "" {
		return fmt.Errorf("must provide container name")
	}
	if nameSet.Has(name) {
		return fmt.Errorf("container name %q duplicated", name)
	}
	nameSet.Insert(name)
	valid := constants.RegexContainerName.MatchString(name)
	if !valid {
		return fmt.Errorf("invalid container name %q, should in format: %v", name, constants.RegexContainerName.String())
	}
	return nil
}

func checkReqVolumes(ctx context.Context, r *cubebox.RunCubeSandboxRequest) error {
	names := map[string]struct{}{}
	for _, v := range r.GetVolumes() {
		if v == nil {
			continue
		}
		if v.GetVolumeSource() == nil {
			continue
		}
		if _, ok := names[v.GetName()]; ok {
			return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "%s duplicated in Volumes params", v.GetName())
		}
		if sp := v.GetVolumeSource().GetSandboxPath(); sp != nil {
			if err := checkSandboxPathVolumeSource(ctx, sp); err != nil {
				return err
			}
		}
		if emptyDir := v.VolumeSource.GetEmptyDir(); emptyDir != nil {
			switch emptyDir.GetMedium() {
			case cubebox.StorageMedium_StorageMediumDefault,
				cubebox.StorageMedium_StorageMediumMemory,
				cubebox.StorageMedium_StorageMediumCubeMsg:
			default:
				return ret.Err(errorcode.ErrorCode_InvalidParamFormat, "unsupported emptyDir medium in the open source build")
			}
		}
		names[v.GetName()] = struct{}{}
	}

	for _, c := range r.GetContainers() {
		for _, v := range c.GetVolumeMounts() {
			if _, ok := names[v.GetName()]; !ok {
				return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "volume %s not found", v.GetName())
			}
		}
	}

	return nil
}

func checkSandboxPathVolumeSource(ctx context.Context, v *cubebox.SandboxPathVolumeSource) error {
	if v.GetType() == "" {
		return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "sandbox path type should not be empty")
	}
	if v.GetPath() == "" {
		return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "sandbox path should not be empty")
	}
	switch v.GetType() {
	case cubebox.SandboxPathType_Cgroup.String(), cubebox.SandboxPathType_Directory.String(), cubebox.SandboxPathType_SharedBindMount.String():
	default:
		return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "sandbox path type %s not supported", v.GetType())
	}

	return nil
}

func checkContainerVolumes(ctx context.Context, c *cubebox.ContainerConfig) error {
	if c.GetVolumeMounts() == nil {
		return nil
	}
	for _, v := range c.GetVolumeMounts() {
		if v.GetContainerPath() == "" {
			return ret.Errorf(errorcode.ErrorCode_InvalidParamFormat, "%s should provide container_path", v.GetName())
		}
	}
	return nil
}

func checkResource(ctx context.Context, containerReq *cubebox.ContainerConfig) error {
	if containerReq.GetResources() == nil {
		return fmt.Errorf("should provide resources param")
	}

	if cpuStr := containerReq.GetResources().GetCpu(); cpuStr == "" {
		return fmt.Errorf("should provide resources.cpu param")
	} else {
		_, err := resource.ParseQuantity(cpuStr)
		if err != nil {
			return fmt.Errorf("invalid resources.cpu param")
		}
	}

	if memStr := containerReq.GetResources().GetMem(); memStr == "" {
		return fmt.Errorf("should provide resources.mem param")
	} else {
		_, err := resource.ParseQuantity(memStr)
		if err != nil {
			return fmt.Errorf("invalid resources.mem param")
		}
	}
	return nil
}

func checkProbe(ctx context.Context, containerReq *cubebox.ContainerConfig) error {
	if containerReq.GetProbe() != nil {
		if containerReq.GetProbe().GetProbeHandler() == nil {
			return fmt.Errorf("should provide probe.probe_handler param")
		}
		if containerReq.GetProbe().GetProbeHandler().GetTcpSocket() != nil {
			if port := containerReq.GetProbe().GetProbeHandler().GetTcpSocket().GetPort(); port < 0 {
				return fmt.Errorf("invalid probe port[%v]", port)
			}
		} else if containerReq.GetProbe().GetProbeHandler().GetPing() != nil {
		} else if containerReq.GetProbe().GetProbeHandler().GetHttpGet() != nil {
		} else {
			return fmt.Errorf("invalid probe.probe_handler  param")
		}

		if containerReq.GetProbe().TimeoutMs <= 0 {
			return fmt.Errorf("invalid probe.timeout_ms[%v]",
				containerReq.GetProbe().TimeoutMs)
		}
	}
	return nil
}

func debugStdout(ctx context.Context, id string) {
	logEntry := CubeLog.GetLogger("stdout").WithContext(ctx)
	fields := CubeLog.Fields{"Module": "Stdout", "ContainerId": id}
	shimLog := logEntry.WithFields(fields)
	log := log.G(ctx).WithFields(fields)
	log.Debug("run debugStdout")
	recov.GoWithRecover(
		func() {
			stdoutFifo := taskio.GetFIFOFile(id)
			f, err := fifo.OpenFifo(context.Background(), stdoutFifo, syscall.O_RDONLY|syscall.O_CREAT|syscall.O_NONBLOCK, 0700)
			if err != nil {
				log.Errorf("%s OpenFifo err:%v", err)
				return
			}
			defer f.Close()
			const readBufSize = 4096
			var logBuf taskio.LogBuffer
			for {
				var buf [readBufSize]byte
				n, err := f.Read(buf[:])
				if err == nil {
					lines := logBuf.Write(buf[:n])
					for _, line := range lines {

						shimLog.Errorf(string(line))
					}
					continue
				}
				if err == io.EOF {
					log.Debug("read EOF from %s, read logs done", stdoutFifo)
				} else {
					log.Errorf("read %s failed: %+v", stdoutFifo, err)
				}
				break
			}
			log.Debug("end copy: %s", stdoutFifo)
		})
}

func transformError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "was not found") {
		return ret.Err(errorcode.ErrorCode_InitCommandPathError, err.Error())
	}
	if strings.Contains(err.Error(), "exec file: No such file or directory") {
		return ret.Err(errorcode.ErrorCode_InitCommandPathError, err.Error())
	}
	if strings.Contains(err.Error(), "exec file: Permission denied") {
		return ret.Err(errorcode.ErrorCode_InitCommandPathError, err.Error())
	}
	return ret.Err(errorcode.ErrorCode_NewTaskFailed, err.Error())
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	cubeboximages "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/dao"
	dbmodels "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/ret"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/utils"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
)

const maxCreateTimeEnvVarsAnnotationBytes = 16 * 1024

func checkAndGetReqResource(req *types.CreateCubeSandboxReq) (*selctx.RequestResource, error) {
	res := &selctx.RequestResource{
		Cpu: resource.MustParse("0"),
		Mem: resource.MustParse("0"),
	}
	cpu, mem, err := getReqResource(req)
	if err != nil {
		return nil, err
	}

	res.Cpu = cpu
	res.Mem = mem

	if req.Annotations[constants.CubeAnnotationsSystemDiskSize] != "" {
		var err error
		res.SystemDiskSize, err = strconv.ParseInt(req.Annotations[constants.CubeAnnotationsSystemDiskSize], 10, 64)
		if err != nil {
			return nil, ret.Errorf(errorcode.ErrorCode_MasterParamsError, "%s", err)
		}
	}
	if req.Annotations[constants.CubeAnnotationsFallbackToSlowPath] == "true" {
		res.EnableSlowPath = true
	}

	if req.Annotations != nil {
		_, hasTemplateID := req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID]
		hasTemplateVersion := constants.HasAppSnapshotTemplateVersion(req.Annotations)
		if hasTemplateID && hasTemplateVersion {

			res.TemplateID = req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID]
			if len(req.DistributionScope) > 0 {
				res.TemplateNodeScope = append([]string(nil), req.DistributionScope...)
				res.EnforceSnapshotStorage = true
			}
		}
	}

	return res, nil
}

func checkParam(req *types.CreateCubeSandboxReq) error {
	if req.Request == nil {
		return ret.Err(errorcode.ErrorCode_MasterParamsError, "requestID is nil")
	}

	if len(req.Containers) == 0 {
		return ret.Err(errorcode.ErrorCode_MasterParamsError, "containers param is nil")
	}

	return nil
}

func getReqResource(req *types.CreateCubeSandboxReq) (cpu, mem resource.Quantity, err error) {
	cpu = resource.MustParse("0")
	mem = resource.MustParse("0")

	for _, ctr := range req.Containers {
		if ctr.Resources == nil {
			err = ret.Err(errorcode.ErrorCode_MasterParamsError, "request Resources nil")
			break
		}
		ctncpuQuantity, err := resource.ParseQuantity(ctr.Resources.Cpu)
		if err != nil {
			err = fmt.Errorf("parse container %q cpu limit: %w", ctr.Name, err)
			break
		}
		ctnmemQuantity, err := resource.ParseQuantity(ctr.Resources.Mem)
		if err != nil {
			err = fmt.Errorf("parse container %q mem limit: %w", ctr.Name, err)
			break
		}
		cpu.Add(ctncpuQuantity)
		mem.Add(ctnmemQuantity)
	}

	if err != nil {
		return cpu, mem, err
	}

	if config.GetConfig().Scheduler != nil {
		if cpu.Cmp(config.GetConfig().Scheduler.MaxMvmCPURes()) >= 0 {
			return cpu, mem, ret.Errorf(errorcode.ErrorCode_MasterParamsError, "request Resources cpu[%dm] is invalid",
				cpu.MilliValue())
		}
		if mem.Cmp(config.GetConfig().Scheduler.MaxMvmMemoryRes()) >= 0 {
			return cpu, mem, ret.Errorf(errorcode.ErrorCode_MasterParamsError, "request Resources  mem[%dKB] is invalid",
				mem.Value()/1024)
		}
	}
	return cpu, mem, err
}

// resolveTimeoutSeconds normalizes client timeout + server default.
// See docs/guide/lifecycle.md — Timeout semantics (canonical).
func resolveTimeoutSeconds(clientTimeout *int, serverDefault int) (int, error) {
	switch {
	case clientTimeout == nil:
		if serverDefault > 0 {
			return serverDefault, nil
		}
		return types.NeverTimeout, nil
	case *clientTimeout < 0:
		return types.NeverTimeout, nil
	default:
		return *clientTimeout, nil
	}
}

func ConstructCubeletReq(ctx context.Context, req *types.CreateCubeSandboxReq) (*cubebox.RunCubeSandboxRequest, error) {
	if err := checkParam(req); err != nil {
		return nil, err
	}
	log.G(ctx).Infof("[hostdir] ConstructCubeletReq: annotations=%v volumes_before_inject=%d",
		req.Annotations, len(req.Volumes))

	// Normalize the sandbox idle timeout into a concrete value.
	timeoutSeconds, err := resolveTimeoutSeconds(req.Timeout, config.GetConfig().CubeletConf.DefaultTimeoutInsec)
	if err != nil {
		return nil, err
	}
	req.Timeout = &timeoutSeconds

	out := &cubebox.RunCubeSandboxRequest{
		RequestID:         req.RequestID,
		Labels:            req.Labels,
		InstanceType:      req.InstanceType,
		NetworkType:       req.NetworkType,
		Annotations:       make(map[string]string),
		RuntimeHandler:    req.RuntimeHandler,
		Namespace:         req.Namespace,
		CubeNetworkConfig: mapCubeNetworkConfig(req.CubeNetworkConfig),
	}
	log.G(ctx).Infof(
		"ConstructCubeletReq: instance_type=%s network_type=%s cube_network_config=%s",
		req.InstanceType,
		req.NetworkType,
		formatConstructCubeNetworkConfig(out.CubeNetworkConfig),
	)

	err = checkAndGetAnnotation(req, out)
	if err != nil {
		return nil, ret.Err(errorcode.ErrorCode_MasterParamsError, err.Error())
	}

	if err = injectHostDirMounts(ctx, req); err != nil {
		return nil, ret.Err(errorcode.ErrorCode_MasterParamsError, err.Error())
	}
	log.G(ctx).Infof("[hostdir] ConstructCubeletReq: volumes_after_inject=%d", len(req.Volumes))

	if err = injectPluginVolumeMounts(ctx, req); err != nil {
		return nil, ret.Err(errorcode.ErrorCode_MasterParamsError, err.Error())
	}

	if err = checkAndGetVolumes(req, out); err != nil {
		return nil, ret.Err(errorcode.ErrorCode_MasterParamsError, err.Error())
	}
	// Sync any annotations added by checkAndGetVolumes (e.g. plugin-volume-sources)
	// into the outgoing RunCubeSandboxRequest.
	for k, v := range req.Annotations {
		if _, exists := out.Annotations[k]; !exists {
			out.Annotations[k] = v
		}
	}

	if err = checkAndGetContainers(req, out); err != nil {
		return nil, ret.Err(errorcode.ErrorCode_MasterParamsError, err.Error())
	}

	if err = getExposedPorts(req, out); err != nil {
		return nil, ret.Err(errorcode.ErrorCode_MasterParamsError, err.Error())
	}

	return out, nil
}

func mapCubeNetworkConfig(in *types.CubeNetworkConfig) *cubebox.CubeNetworkConfig {
	if in == nil {
		return nil
	}
	out := &cubebox.CubeNetworkConfig{
		AllowOut: append([]string(nil), in.AllowOut...),
		DenyOut:  append([]string(nil), in.DenyOut...),
		Rules:    mapEgressRules(in.Rules),
	}
	if in.AllowInternetAccess != nil {
		allowInternetAccess := *in.AllowInternetAccess
		out.AllowInternetAccess = &allowInternetAccess
	}
	return out
}

func mapEgressRules(in []*types.EgressRule) []*cubebox.EgressRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]*cubebox.EgressRule, 0, len(in))
	for _, r := range in {
		if r == nil {
			continue
		}
		out = append(out, &cubebox.EgressRule{
			Name:   r.Name,
			Match:  mapEgressRuleMatch(r.Match),
			Action: mapEgressRuleAction(r.Action),
		})
	}
	return out
}

func mapEgressRuleMatch(in *types.EgressRuleMatch) *cubebox.EgressRuleMatch {
	if in == nil {
		return nil
	}
	return &cubebox.EgressRuleMatch{
		Sni:    in.SNI,
		Host:   in.Host,
		Method: append([]string(nil), in.Method...),
		Path:   in.Path,
		Scheme: in.Scheme,
	}
}

func mapEgressRuleAction(in *types.EgressRuleAction) *cubebox.EgressRuleAction {
	if in == nil {
		return nil
	}
	out := &cubebox.EgressRuleAction{
		Allow: in.Allow,
		Audit: in.Audit,
	}
	if len(in.Inject) > 0 {
		out.Inject = make([]*cubebox.EgressRuleInject, 0, len(in.Inject))
		for _, inj := range in.Inject {
			if inj == nil {
				continue
			}
			out.Inject = append(out.Inject, &cubebox.EgressRuleInject{
				Header: inj.Header,
				Secret: inj.Secret,
				Format: inj.Format,
			})
		}
	}
	return out
}

func formatConstructCubeNetworkConfig(in *cubebox.CubeNetworkConfig) string {
	if in == nil {
		return "allow_internet_access=default(true) allow_out=[] deny_out=[] rules=0"
	}
	allowInternetAccess := "default(true)"
	if in.AllowInternetAccess != nil {
		allowInternetAccess = fmt.Sprintf("%t", in.GetAllowInternetAccess())
	}
	return fmt.Sprintf("allow_internet_access=%s allow_out=%v deny_out=%v rules=%d", allowInternetAccess, in.GetAllowOut(), in.GetDenyOut(), len(in.GetRules()))
}

func getExposedPorts(req *types.CreateCubeSandboxReq, out *cubebox.RunCubeSandboxRequest) error {

	if config.GetConfig().CubeletConf.EnableExposedPort {
		var tmpExposedPorts []string

		if ports, ok := req.Annotations[constants.AnnotationsExposedPort]; ok {
			tmpExposedPorts = strings.Split(ports, ":")
		} else {

			tmpExposedPorts = config.GetConfig().CubeletConf.ExposedPortList
		}

		for _, p := range tmpExposedPorts {
			v, err := strconv.ParseInt(p, 10, 64)
			if err != nil {
				return ret.Errorf(errorcode.ErrorCode_MasterParamsError, "com.exposed_ports:%s,invalid:%v", p, err.Error())
			}
			out.ExposedPorts = append(out.ExposedPorts, v)
		}
		if len(out.GetExposedPorts()) <= 0 {
			return ret.Errorf(errorcode.ErrorCode_MasterParamsError, "com.exposed_ports is empty")
		}
	}
	return nil
}
func checkAndGetContainers(req *types.CreateCubeSandboxReq, out *cubebox.RunCubeSandboxRequest) error {

	for _, cnt := range req.Containers {
		if cnt.Resources == nil {
			return errors.New("request Resources nil")
		}
		if cnt.Image == nil {
			return errors.New("request Image nil")
		}
		if cnt.SecurityContext == nil {
			cnt.SecurityContext = &types.ContainerSecurityContext{}
		}
		c := &cubebox.ContainerConfig{
			Id:         cnt.Id,
			Name:       cnt.Name,
			Command:    cnt.Command,
			Args:       cnt.Args,
			WorkingDir: cnt.WorkingDir,
			Resources: &cubebox.Resource{
				Cpu: cnt.Resources.Cpu,
				Mem: cnt.Resources.Mem,
			},
			Sysctls:     cnt.Sysctls,
			Annotations: map[string]string{},
		}
		if err := checkAndGetImageSpec(c, cnt); err != nil {
			return err
		}
		if cnt.Resources.Limit != nil {
			c.Resources.CpuLimit = cnt.Resources.Limit.Cpu
			c.Resources.MemLimit = cnt.Resources.Limit.Mem
		}
		if cnt.RLimit != nil {
			c.RLimit = &cubebox.RLimit{
				NoFile: cnt.RLimit.NoFile,
			}
		}
		for _, e := range cnt.Envs {
			c.Envs = append(c.Envs, &cubebox.KeyValue{Key: e.Key, Value: e.Value})
		}
		if err := checkAndGetSyscalls(c, cnt); err != nil {
			return err
		}
		if err := checkAndGetSecurityContext(c, cnt); err != nil {
			return err
		}
		if err := checkAndGetVolumeMounts(c, cnt, req.Volumes); err != nil {
			return err
		}
		if err := checkAndGethostDns(c, cnt); err != nil {
			return err
		}
		if err := checkAndGetProbe(c, cnt); err != nil {
			return err
		}
		if err := checkAndGetContainerAnnotation(c, cnt); err != nil {
			return err
		}
		if err := checkAndGetContainerHooks(c, cnt); err != nil {
			return err
		}
		out.Containers = append(out.Containers, c)
	}
	return nil
}

func checkAndGetImageSpec(c *cubebox.ContainerConfig, cnt *types.Container) error {
	if cnt.Image == nil {
		return errors.New("request Image nil")
	}
	c.Image = &cubeboximages.ImageSpec{
		Image:        cnt.Image.Image,
		StorageMedia: cnt.Image.StorageMedia,
		Annotations:  cnt.Image.Annotations,
	}
	if cnt.Image.Annotations == nil {
		cnt.Image.Annotations = make(map[string]string)
		c.Image.Annotations = cnt.Image.Annotations
	}
	if cnt.Image.Name != "" {
		c.Image.Annotations[constants.CubeAnnotationsImageName] = cnt.Image.Name
	}
	if cnt.Image.Token != "" {
		c.Image.Annotations[constants.CubeAnnotationsImageToken] = cnt.Image.Token
	}

	if cnt.Image.StorageMedia == "nfs" {
		return errors.New("nfs rootfs images are not supported")
	}
	return nil
}

func checkAndGetContainerAnnotation(c *cubebox.ContainerConfig, cnt *types.Container) error {
	for k, v := range cnt.Annotations {

		if !strings.HasPrefix(k, "cube.") &&
			!strings.HasPrefix(k, constants.AnnotationsNetID) &&
			!strings.HasPrefix(k, constants.AnnotationsInvokePort) &&
			!strings.HasPrefix(k, constants.AnnotationsExposedPort) &&
			!strings.HasPrefix(k, constants.AnnotationsVIPs) {
			c.Annotations[k] = v
		}

		if strings.HasPrefix(k, constants.CubeAnnotationsPrefix) {
			c.Annotations[k] = v
		}

	}
	return nil
}
func checkAndGetSyscalls(c *cubebox.ContainerConfig, cnt *types.Container) error {
	for _, e := range cnt.Syscalls {
		sys := &cubebox.SysCall{
			Names:  e.Names,
			Action: e.Action,
			Errno:  e.Errno,
		}
		if e.Args != nil {
			for _, a := range e.Args {
				sys.Args = append(sys.Args, &cubebox.LinuxSeccompArg{
					Index: a.Index, Value: a.Value, ValueTwo: a.ValueTwo, Op: a.Op,
				})
			}
		}
		c.Syscalls = append(c.Syscalls, sys)

	}

	return nil
}
func checkAndGetSecurityContext(c *cubebox.ContainerConfig, cnt *types.Container) error {
	c.SecurityContext = &cubebox.ContainerSecurityContext{
		ReadonlyRootfs: utils.SafeValue(cnt.SecurityContext.ReadonlyRootfs),
		RunAsUsername:  utils.SafeValue(cnt.SecurityContext.RunAsUsername),
		NoNewPrivs:     utils.SafeValue(cnt.SecurityContext.NoNewPrivs),
		Privileged:     utils.SafeValue(cnt.SecurityContext.Privileged),
	}

	if utils.SafeValue(cnt.SecurityContext).RunAsUser != nil {
		c.SecurityContext.RunAsUser = &cubebox.Int64Value{
			Value: cnt.SecurityContext.RunAsUser.Value,
		}
	}
	if utils.SafeValue(cnt.SecurityContext).RunAsGroup != nil {
		c.SecurityContext.RunAsGroup = &cubebox.Int64Value{
			Value: cnt.SecurityContext.RunAsGroup.Value,
		}
	}
	if utils.SafeValue(cnt.SecurityContext).Capabilities != nil {
		c.SecurityContext.Capabilities = &cubebox.Capability{
			AddCapabilities:        cnt.SecurityContext.Capabilities.AddCapabilities,
			DropCapabilities:       cnt.SecurityContext.Capabilities.DropCapabilities,
			AddAmbientCapabilities: cnt.SecurityContext.Capabilities.AddAmbientCapabilities,
		}
	}
	return nil
}

func checkAndGethostDns(c *cubebox.ContainerConfig, cnt *types.Container) error {
	if cnt.DnsConfig != nil {
		c.DnsConfig = &cubebox.DNSConfig{
			Servers:  cnt.DnsConfig.Servers,
			Searches: cnt.DnsConfig.Searches,
			Options:  cnt.DnsConfig.Options,
		}
	}
	if cnt.HostAliases != nil {
		for _, a := range cnt.HostAliases {
			c.HostAliases = append(c.HostAliases, &cubebox.HostAlias{
				Ip:        a.Ip,
				Hostnames: a.Hostnames,
			})
		}
	}
	return nil
}

func hostDirVolumeNames(volumes []*types.Volume) map[string]bool {
	names := make(map[string]bool)
	for _, v := range volumes {
		if v != nil && v.VolumeSource != nil && v.VolumeSource.HostDirVolumeSources != nil {
			names[v.Name] = true
		}
	}
	return names
}

func checkAndGetVolumeMounts(c *cubebox.ContainerConfig, cnt *types.Container, volumes []*types.Volume) error {
	hdvNames := hostDirVolumeNames(volumes)
	for _, e := range cnt.VolumeMounts {
		if e.Name == "" || e.ContainerPath == "" {
			return errors.New("VolumeMounts error")
		}

		if hdvNames[e.Name] {
			e.Propagation = cubebox.MountPropagation_PROPAGATION_PRIVATE
			e.Exec = false
			if e.Readonly {
				e.RecursiveReadOnly = true
			}
		}
	}
	c.VolumeMounts = cnt.VolumeMounts
	return nil
}

func checkAndGetProbe(c *cubebox.ContainerConfig, cnt *types.Container) error {
	if cnt.Probe != nil {
		if cnt.Probe.ProbeHandler == nil {
			return errors.New("ProbeHandler is nil")
		}
		c.Probe = &cubebox.Probe{
			PeriodMs:         cnt.Probe.PeriodMs,
			SuccessThreshold: cnt.Probe.SuccessThreshold,
			FailureThreshold: cnt.Probe.FailureThreshold,
			ProbeHandler:     &cubebox.ProbeHandler{},
			ProbeTimeoutMs:   cnt.Probe.ProbeTimeoutMs,
		}
		handleProbeHandler(c.GetProbe().GetProbeHandler(), cnt.Probe.ProbeHandler)
		if cnt.Probe.InitialDelaySeconds != 0 {
			c.Probe.InitialDelayMs = cnt.Probe.InitialDelaySeconds * 1000
		}
		if cnt.Probe.TimeoutSeconds != 0 {
			c.Probe.TimeoutMs = cnt.Probe.TimeoutSeconds * 1000
		}
		if cnt.Probe.InitialDelayMs != 0 {
			c.Probe.InitialDelayMs = cnt.Probe.InitialDelayMs
		}
		if cnt.Probe.TimeoutMs != 0 {
			c.Probe.TimeoutMs = cnt.Probe.TimeoutMs
		}
	}
	handlePrestop(&c.Prestop, cnt.Prestop)
	handlePoststop(&c.Poststop, cnt.Poststop)
	return nil
}

func handleProbeHandler(handler *cubebox.ProbeHandler, cntProbeHandler *types.ProbeHandler) {
	if cntProbeHandler.TCPSocket != nil {
		handler.TcpSocket = &cubebox.TCPSocketAction{
			Port: cntProbeHandler.TCPSocket.Port,
			Host: cntProbeHandler.TCPSocket.Host,
		}
	} else if cntProbeHandler.Ping != nil {
		handler.Ping = &cubebox.PingAction{
			Udp: cntProbeHandler.Ping.Udp,
		}
	} else if cntProbeHandler.HttpGet != nil {
		handler.HttpGet = &cubebox.HTTPGetAction{
			Port: cntProbeHandler.HttpGet.Port,
			Host: cntProbeHandler.HttpGet.Host,
			Path: cntProbeHandler.HttpGet.Path,
		}
		if cntProbeHandler.HttpGet.HttpHeaders != nil {
			headers := make([]*cubebox.HTTPHeader, len(cntProbeHandler.HttpGet.HttpHeaders))
			for i, header := range cntProbeHandler.HttpGet.HttpHeaders {
				headers[i] = &cubebox.HTTPHeader{
					Name:  header.Name,
					Value: header.Value,
				}
			}
			handler.HttpGet.HttpHeaders = headers
		}
	}
}

func handlePoststop(dst **cubebox.PostStop, src *types.PostStop) {
	if src != nil && src.LifecyleHandler != nil && src.LifecyleHandler.HttpGet != nil {
		*dst = &cubebox.PostStop{
			TimeoutMs: src.TimeoutMs,
			LifecyleHandler: &cubebox.LifecycleHandler{
				HttpGet: &cubebox.HTTPGetAction{
					Port: src.LifecyleHandler.HttpGet.Port,
					Host: src.LifecyleHandler.HttpGet.Host,
					Path: src.LifecyleHandler.HttpGet.Path,
				},
			},
		}
		if src.LifecyleHandler.HttpGet.HttpHeaders != nil {
			for _, header := range src.LifecyleHandler.HttpGet.HttpHeaders {
				(*dst).LifecyleHandler.HttpGet.HttpHeaders = append((*dst).LifecyleHandler.HttpGet.HttpHeaders,
					&cubebox.HTTPHeader{
						Name:  header.Name,
						Value: header.Value,
					})
			}
		}
	}
}

func handlePrestop(dst **cubebox.PreStop, src *types.PreStop) {
	if src != nil && src.LifecyleHandler != nil && src.LifecyleHandler.HttpGet != nil {
		*dst = &cubebox.PreStop{
			TerminationGracePeriodMs: src.TerminationGracePeriodMs,
			LifecyleHandler: &cubebox.LifecycleHandler{
				HttpGet: &cubebox.HTTPGetAction{
					Port: src.LifecyleHandler.HttpGet.Port,
					Host: src.LifecyleHandler.HttpGet.Host,
					Path: src.LifecyleHandler.HttpGet.Path,
				},
			},
		}
		if src.LifecyleHandler.HttpGet.HttpHeaders != nil {
			for _, header := range src.LifecyleHandler.HttpGet.HttpHeaders {
				(*dst).LifecyleHandler.HttpGet.HttpHeaders = append((*dst).LifecyleHandler.HttpGet.HttpHeaders,
					&cubebox.HTTPHeader{
						Name:  header.Name,
						Value: header.Value,
					})
			}
		}
	}
}
func checkAndGetVolumes(req *types.CreateCubeSandboxReq, out *cubebox.RunCubeSandboxRequest) error {
	// Build a set of plugin volume names from the plugin-volume-mounts annotation.
	pluginVolumeNames := map[string]bool{}
	if raw := req.Annotations[AnnotationPluginVolumeMounts]; raw != "" {
		type mountEntry struct {
			Name string `json:"name"`
		}
		var entries []mountEntry
		if err := json.Unmarshal([]byte(raw), &entries); err == nil {
			for _, e := range entries {
				pluginVolumeNames[e.Name] = true
			}
		}
	}

	if req.Volumes != nil {
		for _, e := range req.Volumes {
			if e.Name == "" {
				return fmt.Errorf("volume name must not be empty")
			}

			v := &cubebox.Volume{
				Name:         e.Name,
				VolumeSource: &cubebox.VolumeSource{},
			}

			// Identify plugin volumes by name appearing in plugin-volume-mounts
			// annotation, or by having a nil VolumeSource.
			if e.VolumeSource == nil || pluginVolumeNames[e.Name] {
				record, err := resolveVolumeRecord(e.Name)
				if err != nil {
					return fmt.Errorf("volume [%s]: %w", e.Name, err)
				}
				if err := appendPluginVolumeSourceAnnotation(req, e.Name, record.Driver); err != nil {
					return fmt.Errorf("volume [%s]: annotation: %w", e.Name, err)
				}
				v.VolumeSource.EmptyDir = &cubebox.EmptyDirVolumeSource{
					SizeLimit: "1Gi",
				}
				out.Volumes = append(out.Volumes, v)
				continue
			}

			if e.VolumeSource.EmptyDir != nil {
				v.VolumeSource.EmptyDir = &cubebox.EmptyDirVolumeSource{
					SizeLimit: e.VolumeSource.EmptyDir.SizeLimit,
					Medium:    cubebox.StorageMedium(e.VolumeSource.EmptyDir.Medium),
				}
			}
			if e.VolumeSource.SandboxPath != nil {
				v.VolumeSource.SandboxPath = &cubebox.SandboxPathVolumeSource{
					Path: e.VolumeSource.SandboxPath.Path,
					Type: e.VolumeSource.SandboxPath.Type,
				}
			}
			if e.VolumeSource.HostDirVolumeSources != nil {
				if err := checkAndGetHostDirVolumeSource(e.VolumeSource.HostDirVolumeSources, v); err != nil {
					return err
				}
			}
			v.VolumeSource.Image = e.VolumeSource.Image
			out.Volumes = append(out.Volumes, v)
		}
	}
	return nil
}

// resolveVolumeRecord looks up a VolumeRecord by volumeID from the DB.
func resolveVolumeRecord(volumeID string) (*dbmodels.VolumeRecord, error) {
	var record dbmodels.VolumeRecord
	err := dao.Default().
		Where("volume_id = ?", volumeID).
		First(&record).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("volume not found: %s", volumeID)
		}
		return nil, fmt.Errorf("db error: %w", err)
	}
	return &record, nil
}

func checkAndGetHostDirVolumeSource(src *types.HostDirVolumeSources, out *cubebox.Volume) error {
	if src == nil {
		return nil
	}
	for _, s := range src.VolumeSources {
		if s.Name == "" {
			return errors.New("host_dir volume source name must not be empty")
		}
		if s.HostPath == "" {
			return fmt.Errorf("host_dir volume source %q: host_path must not be empty", s.Name)
		}
		cleaned, err := validateHostPath(s.HostPath)
		if err != nil {
			return fmt.Errorf("host_dir volume source %q: %w", s.Name, err)
		}
		s.HostPath = cleaned
	}
	out.VolumeSource.HostDirVolumes = &cubebox.HostDirVolumeSources{}
	for _, s := range src.VolumeSources {
		out.VolumeSource.HostDirVolumes.VolumeSources = append(out.VolumeSource.HostDirVolumes.VolumeSources,
			&cubebox.HostDirSource{
				Name:     s.Name,
				HostPath: s.HostPath,
			})
	}
	return nil
}

func checkAndGetAnnotation(req *types.CreateCubeSandboxReq, out *cubebox.RunCubeSandboxRequest) error {
	if req.Annotations == nil {
		return errors.New("annotation param is nil")
	}

	if out.Annotations == nil {
		out.Annotations = make(map[string]string)
	}

	if v, ok := req.Annotations[constants.AnnotationsNetID]; ok {
		if v == "" {
			return errors.New("com.netid param is empty")
		}
	}

	if v, ok := req.Annotations[constants.AnnotationsVIPs]; ok {
		out.Annotations[constants.CubeAnnotationsVIPs] = v
	}

	out.Annotations[constants.CubeAnnotationsBlkQos] = getBlkQosAnnotation(req)
	out.Annotations[constants.CubeAnnotationsFSQos] = getFsQosAnnotation(req)
	for k, v := range req.Annotations {

		if strings.HasPrefix(k, constants.CubeAnnotationsPrefix) ||
			strings.HasPrefix(k, constants.CubeAnnotationsCloadPrefix) {
			out.Annotations[k] = v
		}
	}

	out.Annotations[constants.CubeAnnotationsUseNetFileCache] = "true"

	if v, ok := req.Annotations[constants.CubeAnnotationsInsRegion]; !ok || v == "" {
		out.Annotations[constants.CubeAnnotationsInsRegion] = config.GetConfig().Log.Region
	}
	if err := setCreateTimeEnvVarsAnnotation(out.Annotations, req.CreateTimeEnvVars); err != nil {
		return err
	}
	return nil
}

func setCreateTimeEnvVarsAnnotation(out map[string]string, envVars map[string]string) error {
	if len(envVars) == 0 {
		return nil
	}
	if out == nil {
		return errors.New("annotation output map is nil")
	}
	// Carry the create-time env map to cubelet so the sandbox runtime can
	// initialize envd after startup for envd-backed command execution.
	payload, err := utils.JSONTool.Marshal(envVars)
	if err != nil {
		return fmt.Errorf("marshal create_time_env_vars failed: %w", err)
	}
	if len(payload) > maxCreateTimeEnvVarsAnnotationBytes {
		return fmt.Errorf(
			"create_time_env_vars annotation payload too large: %d bytes exceeds limit %d",
			len(payload),
			maxCreateTimeEnvVarsAnnotationBytes,
		)
	}
	out[constants.CubeAnnotationCreateTimeEnvVars] = string(payload)
	return nil
}

func getBlkQosAnnotation(req *types.CreateCubeSandboxReq) string {
	if config.GetConfig().ExtraConf.BlkQosMap == nil {
		return config.GetConfig().ExtraConf.BlkQos
	}
	qos, ok := config.GetConfig().ExtraConf.BlkQosMap[req.InstanceType]
	if !ok {
		return config.GetConfig().ExtraConf.BlkQos
	}
	return qos
}

func getFsQosAnnotation(req *types.CreateCubeSandboxReq) string {
	if config.GetConfig().ExtraConf.FsQosMap == nil {
		return config.GetConfig().ExtraConf.FsQos
	}
	qos, ok := config.GetConfig().ExtraConf.FsQosMap[req.InstanceType]
	if !ok {
		return config.GetConfig().ExtraConf.FsQos
	}
	return qos
}

func collectMemoryOption(req *types.DeleteCubeSandboxReq, out *cubebox.DestroyCubeSandboxRequest) {
	if config.GetConfig().Common.EnableAllCollectSandboxMemory {
		if out.Annotations == nil {
			out.Annotations = make(map[string]string)
		}
		out.Annotations[constants.CubeAnnotationCollectMemOnExit] = "true"
		return
	}
	if v, ok := req.Annotations[constants.AnnotationsCollectMemOnExit]; ok {
		if out.Annotations == nil {
			out.Annotations = make(map[string]string)
		}
		out.Annotations[constants.CubeAnnotationCollectMemOnExit] = v
	}
}
func safePrintCreateCubeSandboxReq(req *types.CreateCubeSandboxReq) string {
	tmpReq := &types.CreateCubeSandboxReq{}
	tmpData, _ := types.FastestJsoniter.Marshal(req)
	types.FastestJsoniter.Unmarshal(tmpData, tmpReq)
	for _, v := range tmpReq.Volumes {
		if v.VolumeSource == nil {
			continue
		}
		v.VolumeSource = dealSecurity(v.VolumeSource)
	}

	if _, ok := tmpReq.Annotations[constants.CubeAnnotationsInsUserData]; ok {
		tmpReq.Annotations[constants.CubeAnnotationsInsUserData] = "*"
	}

	for _, v := range tmpReq.Containers {
		if v.Image.Token != "" {
			v.Image.Token = "*"
		}
	}

	if tmpReq.CubeNetworkConfig != nil {
		for _, r := range tmpReq.CubeNetworkConfig.Rules {
			if r == nil || r.Action == nil {
				continue
			}
			for _, inj := range r.Action.Inject {
				if inj == nil {
					continue
				}
				if inj.Secret != "" {
					inj.Secret = "***REDACTED***"
				}
			}
		}
	}
	return utils.InterfaceToString(tmpReq)
}

func safePrintCreateCubeSandboxRes(rsp *types.CreateCubeSandboxRes) string {
	if rsp == nil {
		return "<nil>"
	}
	tmp := &types.CreateCubeSandboxRes{
		RequestID:          rsp.RequestID,
		Ret:                rsp.Ret,
		SandboxID:          rsp.SandboxID,
		SandboxIP:          rsp.SandboxIP,
		HostID:             rsp.HostID,
		HostIP:             rsp.HostIP,
		TrafficAccessToken: redactToken(rsp.TrafficAccessToken),
		ExtInfo:            rsp.ExtInfo,
	}
	return utils.InterfaceToString(tmp)
}

// redactToken returns a boolean indicator instead of the raw token value,
// preserving enough context for log triage ("was a token issued?") without
// leaking the secret itself.
func redactToken(token string) string {
	if token == "" {
		return ""
	}
	return "***REDACTED***"
}

func simplePrintCreateCubeSandboxReq(req *types.CreateCubeSandboxReq) string {
	tmp := types.CreateCubeSandboxReq{
		Request: req.Request,
		InsId:   req.InsId,
		InsIp:   req.InsIp,
		Labels:  req.Labels,
	}
	for _, v := range req.Containers {
		tmp.Containers = append(tmp.Containers, &types.Container{
			Name: v.Name,
			Resources: &types.Resource{
				Cpu: v.Resources.Cpu,
				Mem: v.Resources.Mem,
			},
		})
	}
	for _, v := range req.Volumes {
		if v.VolumeSource == nil {
			continue
		}
		if v.VolumeSource.HostDirVolumeSources != nil {
			tmp.Volumes = append(tmp.Volumes, &types.Volume{
				Name: v.Name,
				VolumeSource: &types.VolumeSource{
					HostDirVolumeSources: &types.HostDirVolumeSources{
						VolumeSources: v.VolumeSource.HostDirVolumeSources.VolumeSources,
					},
				},
			})
		}
	}
	return utils.InterfaceToString(tmp)
}

func dealSecurity(volume *types.VolumeSource) *types.VolumeSource {
	if volume == nil {
		return volume
	}

	tmpVolume := &types.VolumeSource{}
	tmpData, _ := types.FastestJsoniter.Marshal(volume)
	types.FastestJsoniter.Unmarshal(tmpData, tmpVolume)
	return tmpVolume
}

func filterErrMsg(code int) bool {
	if config.GetConfig().Common.FilterErrMsgErrorCode == nil {
		return false
	}

	if yes, ok := config.GetConfig().Common.FilterErrMsgErrorCode[code]; ok {
		return yes
	}
	return false
}

func isFilteOut(n *node.Node, disableMap map[string]bool) bool {
	if n == nil {
		return true
	}
	if disableMap == nil {
		return false
	}
	if n.ClusterLabel != "" {
		labels := strings.Split(n.ClusterLabel, ",")
		for _, l := range labels {
			if disableMap[strings.TrimSpace(l)] {
				return true
			}
		}
	}
	return false
}

func checkAndGetContainerHooks(out *cubebox.ContainerConfig, in *types.Container) error {
	if in.Hooks == nil {
		return nil
	}
	out.Hooks = &cubebox.Hooks{}
	if in.Hooks.Prestart != nil {
		for _, h := range in.Hooks.Prestart {
			out.Hooks.Prestart = append(out.Hooks.Prestart, &cubebox.Hook{
				Path:    h.Path,
				Args:    h.Args,
				Env:     h.Env,
				Timeout: h.Timeout,
			})
		}
	}
	return nil
}

// appendPluginVolumeSourceAnnotation records name+driver for a plugin volume
// in the "plugin-volume-sources" annotation so Cubelet can call the right
// Node Hook binary at attach time.
//
// Format: JSON array of {"name": "<volumeID>", "driver": "<driver>"}
func appendPluginVolumeSourceAnnotation(req *types.CreateCubeSandboxReq, name, driver string) error {
	const key = "plugin-volume-sources"
	type entry struct {
		Name   string `json:"name"`
		Driver string `json:"driver"`
	}
	if req.Annotations == nil {
		req.Annotations = make(map[string]string)
	}
	var entries []entry
	if raw := req.Annotations[key]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			return err
		}
	}
	entries = append(entries, entry{Name: name, Driver: driver})
	b, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	req.Annotations[key] = string(b)
	return nil
}

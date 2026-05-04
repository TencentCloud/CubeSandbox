// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"fmt"
	"maps"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"encoding/json"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/events/exchange"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"k8s.io/utils/clock"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

const (
	maxListCubebox = 5000
)

var _ cubebox.CubeboxMgrServer = &service{}

type ServicesConfig struct {
	CreateDeadlineStr string `toml:"create_dead_line"`
	createDeadline    time.Duration

	DestroyDeadlineStr string `toml:"destroy_dead_line"`
	destroyDeadline    time.Duration

	DeadContainerTTLStr string `toml:"dead_container_ttl"`
	deadContainerTTL    time.Duration
}

var (
	defaultCreateDeadline   = 60 * time.Second
	defaultDestroyDeadline  = 60 * time.Second
	defaultDeadContainerTTL = 1 * time.Hour
	cleanerHeartBeat        = 10 * time.Second
)

func defaultServiceConfig() *ServicesConfig {
	return &ServicesConfig{}
}

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.CubeboxServicePlugin,
		ID:     constants.CubeboxServiceID.ID(),
		Config: defaultServiceConfig(),
		Requires: []plugin.Type{
			constants.InternalPlugin,
			constants.WorkflowPlugin,
			plugins.EventPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (_ interface{}, err error) {
			defer func() {
				if err != nil {
					CubeLog.Fatalf("plugin %s init fail:%v", constants.CubeboxServiceID, err.Error())
				}
			}()

			config := ic.Config.(*ServicesConfig)
			t, err := time.ParseDuration(config.CreateDeadlineStr)
			if err != nil || t == 0 {
				config.createDeadline = defaultCreateDeadline
			} else {
				config.createDeadline = t
			}

			t, err = time.ParseDuration(config.DestroyDeadlineStr)
			if err != nil || t == 0 {
				config.destroyDeadline = defaultDestroyDeadline
			} else {
				config.destroyDeadline = t
			}

			t, err = time.ParseDuration(config.DeadContainerTTLStr)
			if err != nil || t == 0 {
				config.deadContainerTTL = defaultDeadContainerTTL
			} else {
				config.deadContainerTTL = t
			}

			CubeLog.Infof("%v init config:%+v",
				fmt.Sprintf("%v.%v", constants.CubeboxServicePlugin, constants.CubeboxServiceID), config)

			ep, err := ic.GetByID(plugins.EventPlugin, "exchange")
			if err != nil {
				return nil, err
			}

			cp, err := ic.GetByID(constants.InternalPlugin, constants.CubeboxID.ID())
			if err != nil {
				return nil, err
			}
			cb, ok := cp.(*local)
			if !ok {
				return nil, fmt.Errorf("not a cubebox manager")
			}

			p, err := ic.GetByID(constants.WorkflowPlugin, constants.WorkflowID.ID())
			if err != nil {
				return nil, err
			}

			e, ok := p.(*workflow.Engine)
			if !ok {
				return nil, fmt.Errorf("not a workflow engine")
			}
			s := &service{
				engine:             e,
				cubeboxMgr:         cb,
				cleaner:            newDeadContainerCleaner(config.deadContainerTTL),
				eventMonitor:       newEventMonitor(cb),
				config:             config,
				events:             ep.(*exchange.Exchange),
				numaNodeIndex:      0,
				updateSandboxLocks: utils.NewResourceLocks(),
				otherRuntime: &ociRuntime{
					cubeboxMgr: cb,
				},
			}

			go func() {
				CubeLog.Info("Start subscribing containerd event")
				s.eventMonitor.subscribe(s.events)
				CubeLog.Info("Start event monitor")
				eventMonitorErrch := s.eventMonitor.start()

				eventMonitorErr := <-eventMonitorErrch

				s.eventMonitor.stop()

				if eventMonitorErr != nil {
					CubeLog.Errorf("Event monitor exit: %v", eventMonitorErr)
				}
			}()

			go s.destroyDeadContainers()

			return s, nil
		},
	})
}

type service struct {
	config       *ServicesConfig
	cubeboxMgr   *local
	cleaner      *deadContainerCleaner
	eventMonitor *eventMonitor
	engine       *workflow.Engine
	events       *exchange.Exchange
	cubebox.UnimplementedCubeboxMgrServer
	numaNodeIndex      uint32
	updateSandboxLocks *utils.ResourceLocks

	otherRuntime *ociRuntime
}

func (s *service) RegisterTCP(server *grpc.Server) error {
	cubebox.RegisterCubeboxMgrServer(server, s)
	return nil
}

func (s *service) Register(server *grpc.Server) error {
	cubebox.RegisterCubeboxMgrServer(server, s)
	return nil
}

func safePrint(req *cubebox.RunCubeSandboxRequest) string {
	tmpReq := &cubebox.RunCubeSandboxRequest{}
	body, _ := json.Marshal(req)
	json.Unmarshal(body, tmpReq)
	for _, c := range tmpReq.GetContainers() {
		c.Envs = []*cubebox.KeyValue{
			{
				Key:   "__BRIEF_SUMMARY__",
				Value: fmt.Sprintf("There are %d keys. Ignore the verbose envs here.", len(c.Envs)),
			},
		}
	}
	if tmpReq.Annotations != nil {
		tmpReq.Annotations[constants.MasterAnnotationsBlkQos] = "*"
		tmpReq.Annotations[constants.MasterAnnotationsNetWork] = "*"
		tmpReq.Annotations[constants.MasterAnnotationsFSQos] = "*"
		tmpReq.Annotations[constants.MasterAnnotationsUserData] = "*"
	}

	return utils.InterfaceToString(tmpReq)
}
func (s *service) Create(ctx context.Context, req *cubebox.RunCubeSandboxRequest) (*cubebox.RunCubeSandboxResponse, error) {
	rsp := &cubebox.RunCubeSandboxResponse{
		RequestID: req.RequestID,
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
		ExtInfo:   map[string][]byte{},
	}

	if err := checkParam(ctx, req); err != nil {
		rerr, _ := ret.FromError(err)
		rsp.Ret.RetMsg = rerr.Message()
		rsp.Ret.RetCode = rerr.Code()
		return rsp, nil
	}
	SetRunCubeSandboxRequestDefaultValue(req)

	start := time.Now()
	createInfo := &workflow.CreateContext{
		ReqInfo:  req,
		Failover: true,
	}
	ctx = setDefaultContext(ctx, req, createInfo)

	ctx = context.WithValue(ctx, workflow.KCreateContext, createInfo)

	rt := &CubeLog.RequestTrace{
		Action:       "Create",
		RequestID:    req.RequestID,
		Caller:       constants.CubeboxServiceID.ID(),
		Callee:       s.engine.ID(),
		CalleeAction: "Create",
		FunctionType: createInfo.GetInstanceType(),
		InstanceType: createInfo.GetInstanceType(),
		AppID:        createInfo.GetAppID(),
		Qualifier:    getUserAgent(ctx),
	}
	ctx = CubeLog.WithRequestTrace(ctx, rt)
	if log.IsDebug() {
		log.G(ctx).Debugf("RunCubeSandboxRequest:%s", utils.InterfaceToString(req))
	} else {
		log.G(ctx).Errorf("RunCubeSandboxRequest:%s", safePrint(req))
	}

	s.setRequestResource(createInfo, req)

	defer func() {
		cost := time.Since(start)
		if !ret.IsSuccessCode(rsp.Ret.RetCode) {
			log.G(ctx).WithFields(map[string]interface{}{
				"RetCode": int64(rsp.Ret.RetCode),
			}).Errorf("Create fail:%+v", rsp)
			workflow.RecordCreateMetric(ctx, ret.Err(rsp.Ret.RetCode, rsp.Ret.RetMsg), constants.CubeboxServiceID.ID(), cost)
		} else {
			workflow.RecordCreateMetric(ctx, nil, constants.CubeboxServiceID.ID(), cost)
		}
		dealCreateInnerMetric(rsp, createInfo)

		rt = CubeLog.GetTraceInfo(ctx)
		rt.InstanceID = createInfo.SandboxID
		go s.reportTrace(rt, createInfo.GetMetric())
	}()

	defer recov.HandleCrash(func(panicError interface{}) {
		log.G(ctx).Fatalf("Create panic info:%s, stack:%s", panicError, string(debug.Stack()))
		rsp.Ret.RetMsg = string(debug.Stack())
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
	})

	defer func() {
		rsp.SandboxID = createInfo.SandboxID
		rsp.SandboxIP = getSandboxIp(createInfo)
		rsp.PortMappings = getAllocatedPort(createInfo)

		setCubeExtKey(rsp, createInfo)
	}()

	or, e := s.cubeboxMgr.getSandboxRuntime(req)
	if e != nil {
		log.G(ctx).Errorf("getSandboxRuntime fail:%s", e.Error())
		rsp.Ret.RetMsg = e.Error()
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}
	ctx = constants.WithRuntimeType(ctx, or.Type)
	ns := req.Namespace
	if ns == "" {
		ns = namespaces.Default
	}
	rt.Namespace = ns
	ctx = namespaces.WithNamespace(ctx, ns)
	ctx = workflow.WithCreateContext(ctx, createInfo)
	var createErr error
	if constants.IsCubeRuntime(ctx) {
		createErr = s.engine.Create(ctx, createInfo)
	} else {

		createInfo.SandboxID = utils.GenerateID()
		createErr = s.otherRuntime.Create(ctx, createInfo)
	}

	err, _ := ret.FromError(createErr)
	rsp.Ret.RetMsg = err.Message()
	rsp.Ret.RetCode = err.Code()
	if strings.Contains(rsp.Ret.RetMsg, "no space left") ||
		strings.Contains(rsp.Ret.RetMsg, "No space left") {
		rsp.Ret.RetCode = errorcode.ErrorCode_NoSpaceLeftOnDevice
	}

	if strings.Contains(rsp.Ret.RetMsg, "because File name too long") {
		rsp.Ret.RetCode = errorcode.ErrorCode_SquashfsMountFailed
	}
	return rsp, nil
}

func SetRunCubeSandboxRequestDefaultValue(req *cubebox.RunCubeSandboxRequest) {

	req.InstanceType = constants.GetInstanceTypeWithDefault(req.InstanceType)

	for i := range req.GetContainers() {
		if req.Containers[i].Image == nil {
			req.Containers[i].Image = &images.ImageSpec{
				StorageMedia: images.ImageStorageMediaType_docker.String(),
			}
			continue
		}
		if req.Containers[i].Image.StorageMedia == "" {
			req.Containers[i].Image.StorageMedia = images.ImageStorageMediaType_docker.String()
		}
	}

	if req.NetworkType == "" {
		req.NetworkType = cubebox.NetworkType_tap.String()
	}
}

func setCubeExtKey(rsp *cubebox.RunCubeSandboxResponse, createInfo *workflow.CreateContext) {
	_ = rsp
	_ = createInfo
}

func dealCreateInnerMetric(rsp *cubebox.RunCubeSandboxResponse, createInfo *workflow.CreateContext) {

	shimMetric := time.Duration(0)
	cubeboxMetric := time.Duration(0)
	probeMetric := time.Duration(0)
	serviceMetric := time.Duration(0)
	volumeMetric := time.Duration(0)
	for _, m := range createInfo.GetMetric() {
		if m != nil {
			rsp.ExtInfo[m.ID()] = []byte(strconv.FormatInt(m.Duration().Milliseconds(), 10))
			switch m.ID() {
			case constants.CubeboxID.ID():
				cubeboxMetric = m.Duration()
			case constants.CubeboxServiceID.ID():
				serviceMetric = m.Duration()
			case constants.ImagesID.ID(), constants.VolumeSourceID.ID():
				volumeMetric += m.Duration()
			default:
				if strings.HasPrefix(m.ID(), "sandbox") ||
					strings.HasPrefix(m.ID(), "container") {
					shimMetric += m.Duration()
				}
				if strings.Contains(m.ID(), constants.CubeProbeId) {
					probeMetric += m.Duration()
				}
			}
		}
	}
	service_inner := fmt.Sprintf("%s-inner", constants.CubeboxServiceID.ID())

	serviceInnermetric := serviceMetric - probeMetric - volumeMetric
	cuberInnerMetric := cubeboxMetric - shimMetric

	rsp.ExtInfo[constants.CubeInnerId] = []byte(strconv.FormatInt(cuberInnerMetric.Milliseconds(), 10))
	rsp.ExtInfo[service_inner] = []byte(strconv.FormatInt(serviceInnermetric.Milliseconds(), 10))
	er := ret.Err(rsp.Ret.RetCode, rsp.Ret.RetMsg)
	createInfo.AddMetric(er, constants.CubeInnerId, cuberInnerMetric)
	createInfo.AddMetric(er, service_inner, serviceInnermetric)
}

func getProbeDuration(req *cubebox.RunCubeSandboxRequest) time.Duration {
	t := time.Duration(0)
	for _, c := range req.Containers {
		if c.GetProbe() != nil {
			t += time.Duration(c.GetProbe().TimeoutMs) * time.Millisecond
			t += time.Duration(c.GetProbe().InitialDelayMs) * time.Millisecond
		}
	}
	return t
}

func (s *service) reportTrace(rt *CubeLog.RequestTrace, metrics []*workflow.Metric) {
	baseRt := rt.DeepCopy()
	for _, m := range metrics {
		if m != nil {
			err, _ := ret.FromError(m.Error())
			baseRt.Callee = m.ID()
			baseRt.Cost = m.Duration()
			baseRt.RetCode = int64(err.Code())
			baseRt.ErrorCode = CubeLog.CodeSuccess
			if m.Error() == nil && m.Duration() < 5*time.Millisecond {
				continue
			}
			CubeLog.Trace(baseRt)
		}
	}
}

func getSandboxIp(createInfo *workflow.CreateContext) string {
	if createInfo.NetworkInfo == nil {
		return ""
	}
	return createInfo.NetworkInfo.SandboxIP()
}

func getAppID(annotation map[string]string) int64 {
	return 0
}

func getUin(annotation map[string]string) string {
	return ""
}

func getSubUin(annotation map[string]string) string {
	return ""
}

func getAllocatedPort(createInfo *workflow.CreateContext) []*cubebox.PortMapping {
	if createInfo.NetworkInfo == nil {
		return nil
	}
	var res []*cubebox.PortMapping
	for _, p := range createInfo.NetworkInfo.AllocatedPorts() {
		res = append(res, &cubebox.PortMapping{
			ContainerPort: int32(p.ContainerPort),
			HostPort:      int32(p.HostPort),
		})
	}
	return res
}

func (s *service) Destroy(ctx context.Context, req *cubebox.DestroyCubeSandboxRequest) (*cubebox.DestroyCubeSandboxResponse, error) {
	rsp := &cubebox.DestroyCubeSandboxResponse{
		RequestID: req.RequestID,
		SandboxID: req.SandboxID,
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
		ExtInfo:   map[string][]byte{},
	}
	start := time.Now()
	destroyInfo := &workflow.DestroyContext{
		DestroyInfo: req,
		BaseWorkflowInfo: workflow.BaseWorkflowInfo{
			SandboxID: req.SandboxID,
		},
	}
	ctx = context.WithValue(ctx, workflow.KDestroyContext, destroyInfo)
	ua := getUserAgent(ctx)

	rt := &CubeLog.RequestTrace{
		Action:       "Destroy",
		RequestID:    req.RequestID,
		Caller:       constants.CubeboxServiceID.ID(),
		Callee:       s.engine.ID(),
		CalleeAction: "Destroy",
		InstanceID:   req.SandboxID,
		FunctionType: destroyInfo.GetInstanceType(),
		InstanceType: destroyInfo.GetInstanceType(),
		AppID:        getAppID(req.Annotations),
		Qualifier:    ua,
	}
	ctx = CubeLog.WithRequestTrace(ctx, rt)
	stepLog := log.G(ctx).WithFields(CubeLog.Fields{
		"cubeboxID": req.SandboxID,
		"step":      "cubeboxDestroy",
	})
	log.G(ctx).Errorf("DestroyCubeSandboxRequest:%+v", req)

	defer func() {
		if !ret.IsSuccessCode(rsp.Ret.RetCode) {
			stepLog.WithFields(map[string]interface{}{
				"RetCode": int64(rsp.Ret.RetCode),
			}).Errorf("Destroy fail:%+v", rsp)
			workflow.RecordDestroyMetric(ctx, ret.Err(rsp.Ret.RetCode, rsp.Ret.RetMsg),
				constants.CubeboxServiceID.ID(), time.Since(start))
		} else {
			workflow.RecordDestroyMetric(ctx, nil, constants.CubeboxServiceID.ID(), time.Since(start))
			stepLog.Infof("destroy cubebox success")
		}
		dealDestroyInnerMetric(rsp, destroyInfo)

		go s.reportTrace(CubeLog.GetTraceInfo(ctx), destroyInfo.GetMetric())
	}()

	defer recov.HandleCrash(func(panicError interface{}) {
		stepLog.Fatalf("Destroy panic info:%s, stack:%s", panicError, string(debug.Stack()))
		rsp.Ret.RetMsg = string(debug.Stack())
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
	})

	if req.SandboxID == "" {
		rsp.Ret.RetMsg = "container name not found in DestroyCubeSandboxRequest"
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}

	ns := namespaces.Default
	cb, er := s.cubeboxMgr.cubeboxManger.Get(ctx, req.SandboxID)
	if er == nil {
		ns = cb.Namespace

		if cb.RequestSource != "" {
			ua := getUserAgent(ctx)
			stepLog = stepLog.WithFields(CubeLog.Fields{"user-agent": ua})
			if ua != cb.RequestSource {
				log.G(ctx).Warnf("Illegal deletion: user agent %q is not equal to the user agent in cubebox %q", ua, cb.RequestSource)
			}
		}

		if cb.UserMarkDeletedTime == nil {
			now := time.Now()
			cb.UserMarkDeletedTime = &now
			cb.DeleteRequestID = req.RequestID
			s.cubeboxMgr.cubeboxManger.SyncByID(ctx, req.SandboxID)
		}
	}
	ctx = namespaces.WithNamespace(ctx, ns)

	if req.GetAnnotations() != nil {
		if set := req.GetAnnotations()["cube.debug.shim_create_fail"]; set == "true" {
			ctx = context.WithValue(ctx, "debug_create_fail", set)
		}
		if set := req.GetAnnotations()["cube.debug.destroy_forcibly"]; set == "true" {
			ctx = context.WithValue(ctx, "destroy_forcibly", set)
		}
		if set := req.GetAnnotations()[constants.AnnotationCollectMemOnExit]; set == "true" {
			ctx = constants.WithCollectMemory(ctx)
		}
		if set := req.GetAnnotations()["cube.debug.cleanup"]; set == "true" {
			cleanOpts := &workflow.CleanContext{
				BaseWorkflowInfo: workflow.BaseWorkflowInfo{
					SandboxID: req.SandboxID,
				},
			}
			if err := s.engine.CleanUp(ctx, cleanOpts); err != nil {
				log.G(ctx).Fatalf("CleanUp:%s", err)
			}
			return rsp, nil
		}
	}

	ctx = log.WithLogger(ctx, stepLog)
	ctx, cancel := context.WithTimeout(ctx, s.config.destroyDeadline)
	defer cancel()
	if sb, err := s.cubeboxMgr.cubeboxManger.Get(ctx, req.SandboxID); err == nil {
		runtimeType := s.cubeboxMgr.config.DefaultRuntimeName
		if sb.OciRuntime != nil {
			runtimeType = sb.OciRuntime.Type
		}
		ctx = constants.WithRuntimeType(ctx, runtimeType)

		if !constants.IsCubeRuntime(ctx) {
			err = s.otherRuntime.cubeboxMgr.Destroy(ctx, destroyInfo)
			if err != nil {
				rsp.Ret.RetMsg = err.Error()
				rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
			} else {
				rsp.Ret.RetMsg = "success"
				rsp.Ret.RetCode = errorcode.ErrorCode_Success
			}
			return rsp, nil
		}
	}

	err, _ := ret.FromError(s.engine.Destroy(ctx, destroyInfo))
	rsp.Ret.RetMsg = err.Message()
	rsp.Ret.RetCode = err.Code()
	return rsp, nil
}

func dealDestroyInnerMetric(rsp *cubebox.DestroyCubeSandboxResponse, destroyInfo *workflow.DestroyContext) {

	shimMetric := time.Duration(0)
	cubeboxMetric := time.Duration(0)
	prestopMetric := time.Duration(0)
	serviceMetric := time.Duration(0)
	for _, m := range destroyInfo.GetMetric() {
		if m != nil {
			rsp.ExtInfo[m.ID()] = []byte(strconv.FormatInt(m.Duration().Milliseconds(), 10))
			switch m.ID() {
			case constants.CubeboxID.ID():
				cubeboxMetric = m.Duration()
			case constants.CubeboxServiceID.ID():
				serviceMetric = m.Duration()
			default:
				if strings.HasPrefix(m.ID(), "sandbox") ||
					strings.HasPrefix(m.ID(), "container") {
					shimMetric += m.Duration()
				}
				if strings.Contains(m.ID(), constants.CubePrestopId) {
					prestopMetric += m.Duration()
				}
			}
		}
	}
	service_inner := fmt.Sprintf("%s-inner", constants.CubeboxServiceID.ID())
	serverInnerMetric := serviceMetric - prestopMetric
	rsp.ExtInfo[service_inner] = []byte(strconv.FormatInt(serverInnerMetric.Milliseconds(), 10))
	rsp.ExtInfo[constants.CubeInnerId] = []byte(strconv.FormatInt((cubeboxMetric - shimMetric).Milliseconds(),
		10))
	er := ret.Err(rsp.Ret.RetCode, rsp.Ret.RetMsg)
	destroyInfo.AddMetric(er, constants.CubeInnerId, cubeboxMetric-shimMetric)
	destroyInfo.AddMetric(er, service_inner, serverInnerMetric)
}

func setDefaultContext(ctx context.Context, req *cubebox.RunCubeSandboxRequest,
	createInfo *workflow.CreateContext) context.Context {
	if req.GetAnnotations() != nil {
		if set := req.GetAnnotations()["cube.debug.disable_failover"]; set == "true" {
			createInfo.Failover = false
		}
		if set := req.GetAnnotations()[constants.MasterAnnotationsDisableVmCgroup]; set == "true" {
			ctx = constants.WithDisableVMCgroup(ctx, true)
		}
		if set := req.GetAnnotations()[constants.MasterAnnotationsDisableHostCgroup]; set == "true" {
			ctx = constants.WithDisableHostCgroup(ctx, true)
		}
	}
	return ctx
}

func (s *service) List(ctx context.Context, req *cubebox.ListCubeSandboxRequest) (res *cubebox.ListCubeSandboxResponse, retErr error) {
	defer recov.HandleCrash(func(panicError interface{}) {
		log.G(ctx).Fatalf("List panic info:%s, stack:%s", panicError, string(debug.Stack()))
		retErr = fmt.Errorf("internal error")
	})
	res = &cubebox.ListCubeSandboxResponse{}

	cubeboxInStore := s.cubeboxMgr.cubeboxManger.List()
	if len(cubeboxInStore) > maxListCubebox {

		sort.Slice(cubeboxInStore, func(i, j int) bool {
			if cubeboxInStore[i].Status.IsTerminated() {
				return false
			}
			return cubeboxInStore[i].CreatedAt > cubeboxInStore[j].CreatedAt
		})
		cubeboxInStore = cubeboxInStore[:maxListCubebox]
	}
	var cbs []*cubebox.CubeSandbox
	for _, cb := range cubeboxInStore {
		cbs = append(cbs, toGRPCCubeBox(cb, req.GetOption()))
	}

	cbs = s.filterGRPCContainer(cbs, req)

	return &cubebox.ListCubeSandboxResponse{Items: cbs}, nil
}

func toGRPCContainer(c *cubeboxstore.Container) *cubebox.Container {
	cc := &cubebox.Container{
		Id:         c.ID,
		Image:      c.Config.GetImage().GetImage(),
		Type:       constants.ContainerTypeContainer,
		Resources:  c.Config.GetResources(),
		State:      c.Status.Get().State(),
		CreatedAt:  c.CreatedAt,
		FinishedAt: c.Status.Get().FinishedAt,
		Labels:     maps.Clone(c.Labels),
	}

	switch cc.State {
	case cubebox.ContainerState_CONTAINER_PAUSING:
		cc.PausedAt = c.Status.Status.PausingAt
	case cubebox.ContainerState_CONTAINER_PAUSED:
		cc.PausedAt = c.Status.Status.PausedAt
	}

	if cc.Labels == nil {
		cc.Labels = make(map[string]string)
	}
	if c.IsPod {
		cc.Labels[constants.ContainerType] = constants.ContainerTypeSandBox
		cc.Type = constants.ContainerTypeSandBox
	} else {
		cc.Labels[constants.ContainerType] = constants.ContainerTypeContainer
	}
	cc.Labels[constants.ContainerName] = c.Name

	checkToImageSelfdefineType(cc, c.Config)

	return cc
}

func deepCopyStringMap(m map[string]string) map[string]string {
	n := make(map[string]string)
	for k, v := range m {
		n[k] = v
	}
	return n
}

func (s *service) filterGRPCContainer(sbs []*cubebox.CubeSandbox, req *cubebox.ListCubeSandboxRequest) []*cubebox.CubeSandbox {
	if req == nil {
		return sbs
	}

	if req.GetId() != "" {
		sb, err := s.cubeboxMgr.cubeboxManger.Get(context.Background(), req.GetId())
		if err != nil {
			return nil
		}
		return []*cubebox.CubeSandbox{toGRPCCubeBox(sb, req.GetOption())}
	}

	return applyCubeSandboxFilter(sbs, req.GetFilter())
}

func applyCubeSandboxFilter(sbs []*cubebox.CubeSandbox, filter *cubebox.CubeSandboxFilter) []*cubebox.CubeSandbox {
	if filter == nil {
		return sbs
	}

	var filterd []*cubebox.CubeSandbox
	for _, sb := range sbs {
		var matchedContainer []*cubebox.Container
		for _, cntr := range sb.Containers {
			if filter.GetState() != nil && filter.GetState().GetState() != cntr.State {
				continue
			}
			if filter.GetLabelSelector() != nil {
				match := true
				for k, v := range filter.GetLabelSelector() {
					got, ok := cntr.Labels[k]
					if !ok || got != v {
						match = false
						break
					}
				}
				if !match {
					continue
				}
			}
			matchedContainer = append(matchedContainer, cntr)
		}

		if len(matchedContainer) == 0 {
			continue
		}

		sb.Containers = matchedContainer
		filterd = append(filterd, sb)
	}

	return filterd
}

func toGRPCCubeBox(box *cubeboxstore.CubeBox, opt *cubebox.ListCubeSandboxOption) *cubebox.CubeSandbox {
	cb := &cubebox.CubeSandbox{
		Id:           box.ID,
		Namespace:    box.Namespace,
		PortMappings: box.PortMappings,
		CreatedAt:    box.CreatedAt,
		Labels:       maps.Clone(box.Labels),
		NumaNode:     box.NumaNode,
	}

	if box.GetStatus() != nil {
		switch box.GetStatus().Get().State() {
		case cubebox.ContainerState_CONTAINER_PAUSING:
			cb.PausedAt = box.GetStatus().Get().PausingAt
		case cubebox.ContainerState_CONTAINER_PAUSED:
			cb.PausedAt = box.GetStatus().Get().PausedAt
		}
	}

	for _, c := range box.AllContainers() {
		cc := toGRPCContainer(c)
		if c.IsPod {
			for key, v := range cb.Labels {
				cc.Labels[key] = v
			}
		}
		cb.Containers = append(cb.Containers, cc)
	}
	if opt != nil && opt.GetPrivateWithCubeboxStore() {
		v, err := json.MarshalIndent(box, "", "    ")
		if err != nil {
			log.L.Errorf("marshal cubebox to json failed: %v", err)
		}
		cb.PrivateCubeboxStorageData = v
	}

	return cb
}

func checkToImageSelfdefineType(c *cubebox.Container, conf *cubebox.ContainerConfig) {
	_ = c
	_ = conf
}

type deadContainerCleaner struct {
	clock clock.Clock

	heartbeat clock.Ticker

	TTL            time.Duration
	systemCapacity int
}

func newDeadContainerCleaner(ttl time.Duration) *deadContainerCleaner {
	clk := &clock.RealClock{}
	return &deadContainerCleaner{
		clock:          clk,
		heartbeat:      clk.NewTicker(cleanerHeartBeat),
		TTL:            ttl,
		systemCapacity: 1000,
	}
}

var deadContainerCount = 0

func (s *service) destroyDeadContainers() {
	defer utils.Recover()
	rt := &CubeLog.RequestTrace{
		Action: "DeadGC",
		Caller: constants.CubeboxServiceID.ID(),
		Callee: s.engine.ID(),
	}

	ctx := CubeLog.WithRequestTrace(context.Background(), rt)
	ctx = log.WithLogger(ctx, log.NewWrapperLogEntry(log.AuditLogger.WithContext(ctx)))
	for range s.cleaner.heartbeat.C() {
		dc := s.cubeboxMgr.cubeboxManger.List()
		if len(dc) == 0 {
			continue
		}

		scanDeadContainer(ctx, dc, s.cubeboxMgr.client, s.cleaner.TTL)
	}
}

func scanDeadContainer(ctx context.Context, dc []*cubeboxstore.CubeBox, client *containerd.Client, ttl time.Duration) {
	now := time.Now()
	var tmpDeadCount = 0
	for _, cb := range dc {
		stepLog := log.G(ctx).WithFields(CubeLog.Fields{
			string(CubeLog.KeyInstanceId): cb.ID,
		})
		if cb.FirstContainer() == nil {
			continue
		}
		// Cubelet itself initiated the pause and already tracks the in-progress
		// state via PausingAt (CONTAINER_PAUSING) or PausedAt (CONTAINER_PAUSED).
		// Calling RecoverContainer -> shim state() while pause_vm() holds the
		// sandbox mutex causes a ttrpc timeout; RecoverContainer then sets
		// Unknown=true, making the sandbox appear Terminated and triggering a
		// spurious Destroy cascade.  Skip paused/pausing sandboxes: DeadGC has
		// nothing useful to do for them.
		if cb.GetStatus() != nil && cb.GetStatus().IsPaused() {
			continue
		}
		ctx = namespaces.WithNamespace(ctx, cb.Namespace)
		ctr, err := cubes.RecoverContainer(ctx, client, cb, cb.FirstContainer())
		if err != nil {
			stepLog.Errorf("recheck container status %s error: %v", cb.FirstContainer().ID, err)
			continue
		}
		if ctr.Status.IsTerminated() {
			if ctr.Status.Status.FinishedAt == 0 {
				ctr.Status.Update(func(s cubeboxstore.Status) (cubeboxstore.Status, error) {
					s.FinishedAt = now.UnixNano()
					return s, nil
				})
			}
			if time.Unix(0, ctr.Status.Status.FinishedAt).Add(ttl).Before(now) {
				stepLog.Warnf("dead container %s is terminating and reach ttl time", ctr.ID)
				tmpDeadCount++
			}
		}
	}
	if deadContainerCount != tmpDeadCount {
		deadContainerCount = tmpDeadCount
		log.G(ctx).Errorf("dead container num %d", deadContainerCount)
	}
}

func getUserAgent(ctx context.Context) string {
	userAgent := constants.UserAgentDefaultValue
	md, ok := metadata.FromIncomingContext(ctx)
	if ok {
		for _, key := range md.Get(constants.UserAgentKey) {
			if key != "" {
				split := strings.Split(key, " ")
				userAgent = split[0]
				break
			}
		}
	}
	return userAgent
}

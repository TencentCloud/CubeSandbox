// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package workflow

import (
	"context"
	"fmt"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/disk"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/container/netfile"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/semaphore"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow/provider"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type ReqContext interface {
	GetInstanceType() string
	GetMetric() []*Metric
	AddMetric(err error, id string, t time.Duration)
	GetSandboxID() string
	GetCPU() int64
	GetMemory() int64
	GetNumaNode() int32
}

var _ ReqContext = &BaseWorkflowInfo{}

type BaseWorkflowInfo struct {
	metricInfo

	SandboxID  string
	CPU        int64
	Memory     int64
	PCIMode    string
	NumaNode   int32
	IsRollBack bool
	AppID      int64
	Uin        string
	SubUin     string
}

func (b *BaseWorkflowInfo) GetSandboxID() string {
	return b.SandboxID
}

func (b *BaseWorkflowInfo) GetInstanceType() string {
	return ""
}

func (b *BaseWorkflowInfo) GetCPU() int64 {
	return b.CPU
}

func (b *BaseWorkflowInfo) GetMemory() int64 {
	return b.Memory
}

func (b *BaseWorkflowInfo) GetNumaNode() int32 {
	return b.NumaNode
}

func (b *BaseWorkflowInfo) GetPCIMode() string {
	return b.PCIMode
}

func (b *BaseWorkflowInfo) IsPCIPFMode() bool {
	return b.PCIMode == constants.PCIModePF
}

func (b *BaseWorkflowInfo) GetAppID() int64 {
	return b.AppID
}

func (b *BaseWorkflowInfo) GetUin() string {
	return b.Uin
}

func (b *BaseWorkflowInfo) GetSubUin() string {
	return b.SubUin
}

type InitInfo struct {
	BaseWorkflowInfo

	NetCIDR    string
	MVMInnerIP net.IP
	TapInitNum int
}

type CreateContext struct {
	BaseWorkflowInfo

	ReqInfo *cubebox.RunCubeSandboxRequest

	NetworkInfo provider.NetworkProvider

	StorageInfo interface{}

	CgroupInfo interface{}

	VolumeInfo interface{}

	Failover bool

	UserData *cubeboxstore.UserData

	CubeBoxCreated bool
	NetFile        *netfile.CubeboxNetfile

	LocalRunTemplate *templatetypes.LocalRunTemplate
}

func (b *CreateContext) GetInstanceType() string {
	if b.ReqInfo == nil {
		return ""
	}
	return b.ReqInfo.InstanceType
}

func (b *CreateContext) IsCreateSnapshot() bool {
	if b.ReqInfo == nil {
		return false
	}
	v, ok := b.ReqInfo.GetAnnotations()[constants.MasterAnnotationsAppSnapshotCreate]
	if !ok || v != "true" {
		return false
	}
	_, ok = b.GetSnapshotTemplateID()
	return ok
}

func (b *CreateContext) IsRetoreSnapshot() bool {
	_, ok := b.GetSnapshotTemplateID()
	if !ok {
		return false
	}
	_, ok = b.ReqInfo.GetAnnotations()[constants.MasterAnnotationsAppSnapshotCreate]
	return !ok
}

func (b *CreateContext) GetSnapshotTemplateID() (string, bool) {
	if b.ReqInfo == nil {
		return "", false
	}
	if b.GetInstanceType() != cubebox.InstanceType_cubebox.String() {
		return "", false
	}
	v, ok := b.ReqInfo.GetAnnotations()[constants.MasterAnnotationAppSnapshotTemplateID]
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

func (b *CreateContext) IsCubeboxV2() bool {
	if b.ReqInfo == nil {
		return false
	}
	return b.ReqInfo.GetAnnotations()[constants.MasterAnnotationAppSnapshotVersion] == "v2"
}

func (b *CreateContext) GetRegion() string {
	if b.ReqInfo == nil {
		return ""
	}
	v, ok := b.ReqInfo.GetAnnotations()[constants.MasterAnnotationsInsRegion]
	if !ok || v == "" {
		return ""
	}
	return v
}

func (b *CreateContext) GetQos(key string) (*disk.RateLimiter, error) {
	return GetQosFromReq(b.ReqInfo, key)
}

type metricInfo struct {
	mu   sync.Mutex
	stat []*Metric
}

func (m *metricInfo) GetMetric() []*Metric {
	return m.stat
}

func (m *metricInfo) AddMetric(err error, id string, t time.Duration) {
	m.mu.Lock()
	m.stat = append(m.stat, &Metric{id: id, err: err, duration: t})
	m.mu.Unlock()
}

type Metric struct {
	id       string
	err      error
	duration time.Duration
}

func (m Metric) ID() string {
	return m.id
}
func (m Metric) Error() error {
	return m.err
}
func (m Metric) Duration() time.Duration {
	return m.duration
}

type DestroyContext struct {
	BaseWorkflowInfo

	DestroyInfo *cubebox.DestroyCubeSandboxRequest
}

func (b *DestroyContext) GetInstanceType() string {
	if b.DestroyInfo == nil || b.DestroyInfo.Filter == nil {
		return ""
	}
	return b.DestroyInfo.Filter.InstanceType
}

type CleanContext struct {
	BaseWorkflowInfo
}

const (
	flow_init    = "init"
	flow_create  = "create"
	flow_destroy = "destroy"
	flow_cleanup = "cleanup"
)

type Flow interface {
	ID() string
	Init(context.Context, *InitInfo) error
	Create(context.Context, *CreateContext) error
	Destroy(context.Context, *DestroyContext) error

	CleanUp(context.Context, *CleanContext) error
}

type Workflow struct {
	Name          string
	MaxConcurrent int64
	Limiter       *semaphore.Limiter
	Steps         []*Step
}

func (w *Workflow) ID() string { return w.Name }

func (w *Workflow) AppendStep(s *Step) { w.Steps = append(w.Steps, s) }

func (w *Workflow) GetOnFlyingRequest() int64 {
	return w.Limiter.Current()
}

func (w *Workflow) GetPeakRequest() int64 {
	return w.Limiter.Peak()
}

func (w *Workflow) SetLimit(limit int64) {
	w.MaxConcurrent = limit

	if oldLimit := w.Limiter.Limit(); oldLimit < 0 {
		CubeLog.Infof("cannot change the limit of unbounded workflow %v", w.Name)
		return
	} else if oldLimit != limit {
		CubeLog.Infof("set workflow %v limit from %v to %v", w.Name, oldLimit, limit)
		w.Limiter.SetLimit(limit)
	}
}

type Step struct {
	Name    string
	Actions []Flow
}

func (s Step) ID() string         { return s.Name }
func (s *Step) AppendFlow(f Flow) { s.Actions = append(s.Actions, f) }

type Engine struct {
	workflows   map[string]*Workflow
	cleanupFlow Flow
}

func (e *Engine) GetFlowOnFlyingRequests(name string) int64 {
	if flow, ok := e.workflows[name]; ok {
		return flow.GetOnFlyingRequest()
	}
	return 0
}

func (e *Engine) GetFlowPeakRequests(name string) int64 {
	if flow, ok := e.workflows[name]; ok {
		return flow.GetPeakRequest()
	}
	return 0
}

func (e *Engine) SetFlowLimit(name string, limit int64) {
	if flow, ok := e.workflows[name]; ok {
		flow.SetLimit(limit)
	}
}

func (e *Engine) AddCleaupFlow(f Flow) {
	e.cleanupFlow = f
}
func (e *Engine) AddFlow(k string, f *Workflow) {
	if e.workflows == nil {
		e.workflows = make(map[string]*Workflow)
	}
	e.workflows[k] = f
}

func (e *Engine) run(do string, ctx context.Context, opts ReqContext) error {
	if flow, ok := e.workflows[do]; ok {

		start := time.Now()
		if !flow.Limiter.TryAcquire() {
			return ret.Errorf(errorcode.ErrorCode_ConcurrentFailed, "flow [%s] exceed limited", flow.ID())
		}
		if flow_create == do {
			rOpts := opts.(*CreateContext)
			rOpts.AddMetric(nil, constants.LimiterId, time.Since(start))
		} else if flow_destroy == do {
			rOpts := opts.(*DestroyContext)
			rOpts.AddMetric(nil, constants.LimiterId, time.Since(start))
		}

		defer flow.Limiter.Release()

		for _, step := range flow.Steps {
			if err := e.parallelRunSteps(do, ctx, opts, step); err != nil {

				if ret.IsErrorCode(err, errorcode.ErrorCode_PreConditionFailed) {
					return err
				}

				if flow_create == do {
					rOpts := opts.(*CreateContext)
					if rOpts.Failover {
						go e.failover(ctx, opts)
					}
				}

				if flow_destroy == do {
					e.cleanUp(ctx, opts)
				}

				return err
			}
			select {
			case <-ctx.Done():

				if ctx.Err() == context.Canceled || ctx.Err() == context.DeadlineExceeded {
					if flow_create == do {
						rOpts := opts.(*CreateContext)
						if rOpts.Failover {
							go e.failover(ctx, opts)
						}
					}
					return ctx.Err()
				}
				return nil
			default:
			}
		}
		return nil

	} else {
		return fmt.Errorf("flow %v not implemented", do)
	}
}

func (e *Engine) cleanUp(ctx context.Context, opts ReqContext) {
	if e.cleanupFlow == nil {
		return
	}
	rt := CubeLog.GetTraceInfo(ctx).DeepCopy()

	rt.InstanceID = opts.GetSandboxID()
	rt.Callee = e.cleanupFlow.ID()
	ctxTmp := CubeLog.WithRequestTrace(ctx, rt)
	ctxTmp = log.ReNewLogger(ctxTmp)
	rOpts := &CreateContext{}
	rOpts.SandboxID = opts.GetSandboxID()
	if err := e.cleanupFlow.Create(ctxTmp, rOpts); err != nil {

		CubeLog.WithContext(ctxTmp).Fatalf("cleanupFlow.Create fatal error:%v", err)
	}
}

func (e *Engine) parallelRunSteps(do string, ctx context.Context, opts ReqContext, step *Step) error {

	eg, ctxWithCancel := errgroup.WithContext(ctx)
	rt := CubeLog.GetTraceInfo(ctx)
	if rt == nil {
		return fmt.Errorf("missing trace info in context")
	}
	for i := range step.Actions {
		flow := step.Actions[i]
		startGo := time.Now()
		eg.Go(func() (err error) {
			defer recov.HandleCrash(func(panicError interface{}) {
				err = ret.Errorf(errorcode.ErrorCode_Unknown, "flow[%v] do %s panic :%v %v", flow.ID(), do, panicError, string(debug.Stack()))
			})
			start := time.Now()
			sRt := rt.DeepCopy()
			sRt.Callee = flow.ID()
			if opts != nil {
				sRt.InstanceID = opts.GetSandboxID()
			}
			ctxTmp := CubeLog.WithRequestTrace(ctxWithCancel, sRt)
			ctxTmp = log.ReNewLogger(ctxTmp)
			switch do {
			case flow_init:
				rOpts := opts.(*InitInfo)
				err = flow.Init(ctxTmp, rOpts)
				rOpts.AddMetric(err, flow.ID(), time.Since(start))
			case flow_create:
				rOpts := opts.(*CreateContext)

				rOpts.AddMetric(err, constants.WorkflowID.ID(), time.Since(startGo))
				err = flow.Create(ctxTmp, rOpts)
				rOpts.AddMetric(err, flow.ID(), time.Since(start))
			case flow_destroy:
				rOpts := opts.(*DestroyContext)

				rOpts.AddMetric(err, constants.WorkflowID.ID(), time.Since(startGo))

				err = flow.Destroy(ctxTmp, rOpts)
				rOpts.AddMetric(err, flow.ID(), time.Since(start))
			case flow_cleanup:
				var rOpts *CleanContext
				if opts != nil {
					rOpts = opts.(*CleanContext)
				}
				err = flow.CleanUp(ctxTmp, rOpts)
			default:
				err = fmt.Errorf("unknown")
			}
			if err != nil {

				CubeLog.WithContext(ctxTmp).Errorf("[%v] fail:%v %v", flow.ID(), ret.FetchErrorCode(err), err)
			}
			return err
		})
	}
	if err := eg.Wait(); err != nil {
		return err

	}
	return nil
}

func (e *Engine) failover(ctx context.Context, opts ReqContext) {
	sandboxID := opts.GetSandboxID()
	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		CubeLog.WithContext(ctx).Fatalf("fail over doing SandboxID:%v,error:%v", sandboxID, err)
		return
	}
	rt := CubeLog.GetTraceInfo(ctx)
	if rt == nil {
		CubeLog.WithContext(ctx).Fatalf("failover: missing trace info SandboxID: %v", sandboxID)
		return
	}

	failoverRt := rt.DeepCopy()
	failoverRt.Caller = e.ID()
	failoverRt.Action = "Destroy"
	failoverRt.CalleeAction = "Failover"
	failoverRt.Callee = e.ID()
	failoverRt.InstanceID = sandboxID

	ctx = namespaces.WithNamespace(context.Background(), namespace)
	ctx, cancel := context.WithTimeout(ctx, config.GetCommon().CommonTimeout)
	defer cancel()

	ctx = CubeLog.WithRequestTrace(ctx, failoverRt)
	destroyOpt := &DestroyContext{
		DestroyInfo: &cubebox.DestroyCubeSandboxRequest{
			RequestID:   uuid.New().String(),
			Annotations: map[string]string{},
		},
	}
	destroyOpt.IsRollBack = true
	if failoverRt.RequestID != "" {
		destroyOpt.DestroyInfo.RequestID = failoverRt.RequestID
	}
	destroyOpt.SandboxID = sandboxID
	destroyOpt.DestroyInfo = &cubebox.DestroyCubeSandboxRequest{
		SandboxID: sandboxID,
		RequestID: rt.RequestID,
	}
	ctx = context.WithValue(ctx, KDestroyContext, destroyOpt)
	ctx = constants.WithFailoverOperation(ctx)
	rOpts := opts.(*CreateContext)
	if rOpts.CubeBoxCreated {
		ctx = constants.WithCubeboxCreated(ctx)
	}
	log.G(ctx).Warnf("fail over doing SandboxID:%v", sandboxID)
	defer func() {
		if err != nil {
			e.cleanUp(ctx, opts)
		}
	}()
	defer recov.HandleCrash(func(panicError interface{}) {
		err = fmt.Errorf("%v", panicError)
		log.G(ctx).Fatalf("fail over Destroy  panic :%v %v", panicError, string(debug.Stack()))
	})
	if err = e.Destroy(ctx, destroyOpt); err != nil {

		log.G(ctx).Fatalf("fail over Destroy fatal error:%v", err)
	}
}

func (s *Engine) ID() string { return constants.WorkflowID.ID() }

func (e *Engine) Init(ctx context.Context, opts *InitInfo) error {
	err := e.run(flow_init, ctx, opts)
	return err
}

func (e *Engine) Create(ctx context.Context, opts *CreateContext) error {
	err := e.run(flow_create, ctx, opts)
	return err
}

func (e *Engine) Destroy(ctx context.Context, opts *DestroyContext) error {
	err := e.run(flow_destroy, ctx, opts)
	return err
}

func (e *Engine) CleanUp(ctx context.Context, opts *CleanContext) error {
	if opts == nil {
		return e.run(flow_cleanup, ctx, nil)
	}
	return e.run(flow_cleanup, ctx, opts)
}

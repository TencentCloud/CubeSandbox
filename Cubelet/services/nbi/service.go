// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nbi

import (
	"context"
	"fmt"
	"os"
	"path"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/containerd/plugin/registry"

	"github.com/containerd/plugin"
	pb "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/nbi/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/trace"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"google.golang.org/grpc"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: constants.CubeboxServicePlugin,
		ID:   "nbi",
		Requires: []plugin.Type{
			constants.InternalPlugin,
			constants.WorkflowPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			p, err := ic.GetByID(constants.WorkflowPlugin, constants.WorkflowID.ID())
			if err != nil {
				return nil, err
			}

			e, ok := p.(*workflow.Engine)
			if !ok {
				return nil, err
			}
			return &CubeAPIServer{engine: e}, nil
		},
	})
}

type CubeAPIServer struct {
	engine *workflow.Engine
	pb.UnimplementedCubeLetServer
}

const InitFlagPath string = "/run/initstatus.dat"

func (s *CubeAPIServer) RegisterTCP(server *grpc.Server) error {
	pb.RegisterCubeLetServer(server, s)
	return nil
}

func (s *CubeAPIServer) Register(server *grpc.Server) error {
	pb.RegisterCubeLetServer(server, s)
	return nil
}

func (s *CubeAPIServer) InitHost(ctx context.Context, in *pb.InitRequest) (*pb.InitResponse, error) {
	rsp := &pb.InitResponse{
		RequestID: in.RequestID,
		Code:      errorcode.ErrorCode_Success, Message: "success",
		ExtInfo: map[string][]byte{},
	}
	start := time.Now()
	InitInfo := &workflow.InitInfo{}
	ctx = context.WithValue(ctx, workflow.KInitContext, InitInfo)

	rt := &CubeLog.RequestTrace{
		Action:       "InitHost",
		RequestID:    in.RequestID,
		Caller:       constants.CubeboxServiceID.ID(),
		Callee:       s.engine.ID(),
		CalleeAction: "Init",
	}
	ctx = CubeLog.WithRequestTrace(ctx, rt)

	defer func() {
		cubelogCode := CubeLog.CodeSuccess
		if !ret.IsSuccessCode(rsp.GetCode()) {
			cubelogCode = CubeLog.CodeInternalError
			CubeLog.WithContext(ctx).Errorf("InitHost fail:%+v", rsp)
			workflow.RecordCreateMetric(ctx, fmt.Errorf("%s", rsp.Message), constants.CubeboxServiceID.ID(), time.Since(start))
		} else {
			workflow.RecordCreateMetric(ctx, nil, constants.CubeboxServiceID.ID(), time.Since(start))
		}
		trace.Report(ctx, s.engine.ID(), "", "InitHost", "Init", time.Since(start),
			rsp.Code, cubelogCode)
		for _, m := range InitInfo.GetMetric() {
			if m != nil {
				rsp.ExtInfo[m.ID()] = []byte(strconv.FormatInt(m.Duration().Milliseconds(), 10))
			}
		}

		go s.reportTrace(ctx, InitInfo.GetMetric())
	}()

	defer recov.HandleCrash(func(panicError interface{}) {
		CubeLog.WithContext(ctx).Fatalf("InitHost panic info:%s, stack:%s", panicError, string(debug.Stack()))
		rsp.Message = string(debug.Stack())
		rsp.Code = errorcode.ErrorCode_Unknown
	})

	if err := s.engine.Init(ctx, InitInfo); err != nil {
		rsp.Message = err.Error()
		rsp.Code = errorcode.ErrorCode_InitHostFailed
		_ = os.RemoveAll(InitFlagPath)
		return rsp, nil
	}

	if err := s.writeInitFlag(); err != nil {
		CubeLog.Errorf("write init flag error")
	}
	return rsp, nil
}

func (s CubeAPIServer) reportTrace(ctx context.Context, metrics []*workflow.Metric) {
	for _, m := range metrics {
		if m != nil {
			cubelogCode := CubeLog.CodeSuccess
			retCode := errorcode.ErrorCode_Success
			if m.Error() != nil {
				cubelogCode = CubeLog.CodeInternalError
				retCode = errorcode.ErrorCode_Unknown
			}
			action, _ := ctx.Value(CubeLog.KeyAction).(string)
			calleeA, _ := ctx.Value(CubeLog.KeyCalleeAction).(string)
			trace.Report(ctx, m.ID(), "", action, calleeA, m.Duration(), retCode, cubelogCode)
		}
	}
}

func (s CubeAPIServer) writeInitFlag() error {
	if exist, _ := utils.DenExist(path.Dir(InitFlagPath)); !exist {
		os.MkdirAll(path.Dir(InitFlagPath), 0755)
	}
	f, err := os.Create(InitFlagPath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, _ = f.WriteString(time.Now().Format("2006-1-2 15:04:05"))
	return nil
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	dynamConf "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/Cubelet/services/images"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

var operationServerStopOnce sync.Once

var hostConfigUpdateMutex sync.Mutex

type OperationServer struct {
	srv                 *http.Server
	engine              *workflow.Engine
	imgExpirationSetter images.ExpirationTimeSetter
	logger              *CubeLog.Entry
	stopCh              chan struct{}
}

func NewOperationServer(engine *workflow.Engine, imgExpirationSetter images.ExpirationTimeSetter) *OperationServer {
	rt := &CubeLog.RequestTrace{
		Action: "Operation",
		Caller: "operation-server",
	}
	ctx := CubeLog.WithRequestTrace(context.Background(), rt)
	logger := CubeLog.WithContext(ctx)

	s := &OperationServer{
		engine:              engine,
		imgExpirationSetter: imgExpirationSetter,
		logger:              logger,
		stopCh:              make(chan struct{}),
	}

	s.srv = &http.Server{
		Handler: http.NewServeMux(),
	}
	return s
}

func (s *OperationServer) Serve(l net.Listener) error {
	if err := s.ApplyHostConfig(); err != nil {
		return err
	}
	return s.srv.Serve(l)
}

func (s *OperationServer) Stop() {
	operationServerStopOnce.Do(func() {
		close(s.stopCh)
	})
}

func (s *OperationServer) doActivateHostConfig(hc *dynamConf.HostConf) error {
	if hc == nil {
		return nil
	}

	if hc.Quota.CreationConcurrentNum > 0 && s.engine != nil {
		s.engine.SetFlowLimit("create", int64(hc.Quota.CreationConcurrentNum))
	}

	if s.imgExpirationSetter == nil {
		return nil
	}

	if hc.GC.CodeExpirationTime != "" {
		t, err := time.ParseDuration(hc.GC.CodeExpirationTime)
		if err != nil || t <= 0 {
			return fmt.Errorf("invalid code expiration time: %w", err)
		}
		if err := s.imgExpirationSetter.SetCodeExpirationTime(t); err != nil {
			return fmt.Errorf("setting code expiration time: %w", err)
		}
	}
	if hc.GC.ImageExpirationTime != "" {
		t, err := time.ParseDuration(hc.GC.ImageExpirationTime)
		if err != nil || t <= 0 {
			return fmt.Errorf("invalid image expiration time: %w", err)
		}
		if err := s.imgExpirationSetter.SetImageExpirationTime(t); err != nil {
			return fmt.Errorf("setting image expiration time: %w", err)
		}
	}

	return nil
}

func (s *OperationServer) ApplyHostConfig() error {
	hostConfigUpdateMutex.Lock()
	defer hostConfigUpdateMutex.Unlock()

	hc := dynamConf.GetHostConf()
	if err := s.doActivateHostConfig(hc); err != nil {
		s.logger.Errorf("failed to apply host config: %v", err)
		return err
	}
	return nil
}

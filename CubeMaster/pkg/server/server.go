// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package server provides the server implementation for the CubeMaster.
package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/cube"
	inner "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/inner"
	metahttp "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/meta"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/middleware"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/notify"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

type Server struct {
	InternalHttpServer *internalHttp
}

func New(ctx context.Context, cfg *config.Config) (*Server, error) {
	if cfg == nil || cfg.Common == nil {
		return nil, errors.New("config is nil")
	}
	s := &Server{}
	var err error
	s.InternalHttpServer, err = NewInternalHttp(ctx, cfg)
	if err != nil {
		return nil, err
	}

	config.AppendConfigWatcher(s)
	return s, nil
}

type internalHttp struct {
	*http.Server
	router *mux.Router
}

func NewInternalHttp(ctx context.Context, cfg *config.Config) (*internalHttp, error) {
	if cfg == nil || cfg.Common == nil {
		return nil, errors.New("config is nil")
	}

	router := mux.NewRouter()
	s := &internalHttp{
		Server: &http.Server{
			Addr:         fmt.Sprintf("0.0.0.0:%d", cfg.Common.HttpPort),
			ReadTimeout:  time.Second * time.Duration(cfg.Common.ReadTimeout),
			WriteTimeout: time.Second * time.Duration(cfg.Common.WriteTimeout),
			IdleTimeout:  time.Second * time.Duration(cfg.Common.IdleTimeout),
			Handler:      router,
		},
		router: router,
	}

	s.registerHandlers()
	return s, nil
}

func (s *internalHttp) registerHandlers() {
	r := s.router

	r.Use(middleware.MiddlewareLogging)

	notifyGroup := r.PathPrefix(notify.NotifyURI()).Subrouter()
	notifyGroup.HandleFunc(notify.HostChangeNotifyAction, notify.HttpHandler).Methods(http.MethodPost)
	notifyGroup.HandleFunc(notify.HealthCheckAction, notify.HttpHandler).Methods(http.MethodGet)

	cubeGroup := r.PathPrefix(cube.CubeURI()).Subrouter()
	cubeGroup.HandleFunc(cube.SandboxAction, cube.HttpHandler).Methods(http.MethodPost, http.MethodDelete)
	cubeGroup.HandleFunc(cube.ImageAction, cube.HttpHandler).Methods(http.MethodPost, http.MethodDelete)
	cubeGroup.HandleFunc(cube.SandboxListAction, cube.HttpHandler).Methods(http.MethodGet, http.MethodPost)
	cubeGroup.HandleFunc(cube.SandboxInfoAction, cube.HttpHandler).Methods(http.MethodGet, http.MethodPost)
	cubeGroup.HandleFunc(cube.SandboxExecAction, cube.HttpHandler).Methods(http.MethodPost)
	cubeGroup.HandleFunc(cube.SandboxUpdateAction, cube.HttpHandler).Methods(http.MethodPost)
	cubeGroup.HandleFunc(cube.SandboxCommitAction, cube.HttpHandler).Methods(http.MethodPost)
	cubeGroup.HandleFunc(cube.TemplateAction, cube.HttpHandler).Methods(http.MethodGet, http.MethodPost, http.MethodDelete)
	cubeGroup.HandleFunc(cube.TemplateRedoAction, cube.HttpHandler).Methods(http.MethodPost)
	cubeGroup.HandleFunc(cube.TemplateBuildStatusAction+"/{build_id}/status", cube.HttpHandler).Methods(http.MethodGet)
	cubeGroup.HandleFunc(cube.TemplateFromImageAction, cube.HttpHandler).Methods(http.MethodGet, http.MethodPost)
	cubeGroup.HandleFunc(cube.TemplateArtifactDownloadAction, cube.HttpHandler).Methods(http.MethodGet, http.MethodHead)
	cubeGroup.HandleFunc(cube.RootfsArtifactAction, cube.HttpHandler).Methods(http.MethodGet)
	cubeGroup.HandleFunc(cube.ListInventoryAction, cube.HttpHandler).Methods(http.MethodPost)

	internalGroup := r.PathPrefix(inner.InnerURI()).Subrouter()
	internalGroup.HandleFunc(inner.NodeAction, inner.HttpHandler).Methods(http.MethodGet)
	internalGroup.HandleFunc(inner.FakeCreateAction, inner.HttpHandler).Methods(http.MethodPost)
	internalGroup.HandleFunc(inner.StateWs, inner.HttpHandler)
	internalGroup.HandleFunc(inner.StateQuery, inner.HttpHandler)

	metaGroup := r.PathPrefix(metahttp.MetaURI()).Subrouter()
	metaGroup.HandleFunc(metahttp.ReadyzAction(), metahttp.ReadyzHandler).Methods(http.MethodGet)
	metaGroup.HandleFunc(metahttp.RegisterNodeAction(), metahttp.RegisterNodeHandler).Methods(http.MethodPost)
	metaGroup.HandleFunc(metahttp.NodesAction(), metahttp.ListNodesHandler).Methods(http.MethodGet)
	metaGroup.HandleFunc(metahttp.NodeAction(), metahttp.GetNodeHandler).Methods(http.MethodGet)
	metaGroup.HandleFunc(metahttp.NodeStatusAction(), metahttp.UpdateNodeStatusHandler).Methods(http.MethodPost)
}

func (s *internalHttp) Start() error {
	if err := s.ListenAndServe(); err != nil {
		if err == http.ErrServerClosed {
			return nil
		}
		return errors.WithStack(err)
	}
	return nil
}

func (s *Server) Run() {
	if s.InternalHttpServer != nil {
		go func() {
			if err := s.InternalHttpServer.Start(); err != nil {
				CubeLog.Errorf("ListenAndServe:%v", err)
			}
		}()
	}
}

func (s *Server) OnEvent(config *config.Config) {
	log.OnChangeConf(config.Log)
}

func (s *Server) Stop() {
	ppid := os.Getpid()
	CubeLog.Errorf("server stopped gracefully begin, pid %v", ppid)
	wg := sync.WaitGroup{}
	recov.GoWithWaitGroup(&wg, func() {
		if s.InternalHttpServer != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := s.InternalHttpServer.Shutdown(ctx); err != nil {
				CubeLog.Fatal("InternalHttp Shutdown:", err)
			}
			select {
			case <-ctx.Done():
				CubeLog.Error("InternalHttp Shutdown timeout")
			default:
				CubeLog.Error("InternalHttp Shutdown succ")
			}
		}
	})
	wg.Wait()
}

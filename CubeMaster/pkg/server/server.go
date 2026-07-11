// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package server provides the server implementation for the CubeMaster.
package server

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/recov"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/common"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/cube"
	inner "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/inner"
	metahttp "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/meta"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/middleware"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/httpservice/notify"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
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
	engine *gin.Engine
}

func NewInternalHttp(ctx context.Context, cfg *config.Config) (*internalHttp, error) {
	if cfg == nil || cfg.Common == nil {
		return nil, errors.New("config is nil")
	}

	engine := gin.New()
	engine.RedirectTrailingSlash = false
	engine.HandleMethodNotAllowed = true
	engine.NoRoute(func(c *gin.Context) {
		rt := CubeLog.GetTraceInfo(c.Request.Context())
		if rt != nil {
			rt.RetCode = -1
		}
		common.WriteResponse(c.Writer, http.StatusOK, &types.Res{
			Ret: &types.Ret{
				RetCode: -1,
				RetMsg:  http.StatusText(http.StatusNotFound),
			},
		})
	})
	engine.NoMethod(func(c *gin.Context) {
		rt := CubeLog.GetTraceInfo(c.Request.Context())
		if rt != nil {
			rt.RetCode = int64(errorcode.ErrorCode_MasterParamsError)
		}
		common.WriteResponse(c.Writer, http.StatusOK, &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  http.StatusText(http.StatusMethodNotAllowed),
			},
		})
	})
	s := &internalHttp{
		Server: &http.Server{
			Addr:         net.JoinHostPort(cfg.Common.HttpBind, strconv.Itoa(cfg.Common.HttpPort)),
			ReadTimeout:  time.Second * time.Duration(cfg.Common.ReadTimeout),
			WriteTimeout: time.Second * time.Duration(cfg.Common.WriteTimeout),
			IdleTimeout:  time.Second * time.Duration(cfg.Common.IdleTimeout),
			Handler:      engine,
		},
		engine: engine,
	}

	s.registerRoutes()
	return s, nil
}

func (s *internalHttp) registerRoutes() {
	r := s.engine
	r.Use(middleware.GinRequestMiddleware())
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	notifyG := r.Group(notify.NotifyURI())
	notify.RegisterNotifyRoutes(notifyG)

	cubeG := r.Group(cube.CubeURI())
	cube.RegisterCubeRoutes(cubeG)

	innerG := r.Group(inner.InnerURI())
	inner.RegisterInnerRoutes(innerG)

	metaG := r.Group(metahttp.MetaURI())
	metahttp.RegisterMetaRoutes(metaG)
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

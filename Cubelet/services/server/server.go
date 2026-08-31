// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package server

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/docker/go-metrics"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/resourcemetrics"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	CubeLog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"google.golang.org/grpc"

	containerdserver "github.com/containerd/containerd/v2/cmd/containerd/server"
	"github.com/containerd/containerd/v2/pkg/sys"
	"github.com/containerd/containerd/v2/pkg/timeout"
	"github.com/containerd/log"
	dynamConf "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/services/images"
	srvconfig "github.com/tencentcloud/CubeSandbox/Cubelet/services/server/config"
	bolt "go.etcd.io/bbolt"
)

const (
	boltOpenTimeout = "io.containerd.timeout.bolt.open"
)

func init() {
	timeout.Set(boltOpenTimeout, 0)
}

func CreateTopLevelDirectories(config *srvconfig.Config) error {
	switch {
	case config.Root == "":
		return errors.New("root must be specified")
	case config.State == "":
		return errors.New("state must be specified")
	case config.Root == config.State:
		return errors.New("root and state must be different paths")
	}

	if err := sys.MkdirAllWithACL(config.Root, 0711); err != nil {
		return err
	}

	if err := sys.MkdirAllWithACL(config.State, 0711); err != nil {
		return err
	}

	if config.TempDir != "" {
		if err := sys.MkdirAllWithACL(config.TempDir, 0711); err != nil {
			return err
		}
		if runtime.GOOS == "windows" {

			os.Setenv("TEMP", config.TempDir)
			os.Setenv("TMP", config.TempDir)
		} else {
			os.Setenv("TMPDIR", config.TempDir)
		}
	}
	return nil
}

func New(ctx context.Context, config *srvconfig.Config) (*Server, error) {

	bolt.DefaultOptions.FreelistType = bolt.FreelistMapType

	baseServer, err := containerdserver.New(ctx, config.Config)
	if err != nil {
		return nil, err
	}

	type operationService interface {
		RegisterOperation(*http.ServeMux) error
	}

	// httpService is for lightweight Cubelet HTTP endpoints exposed by plugins,
	// such as network runtime diagnostics. It is separate from operationService so
	// these handlers are served on Cubelet's normal HTTP listener.
	type httpService interface {
		RegisterHTTP(map[string]http.Handler) error
	}

	type tcpService interface {
		RegisterTCP(*grpc.Server) error
	}

	var (
		operationServices   []operationService
		httpServices        []httpService
		tcpServices         []tcpService
		engine              *workflow.Engine
		imgExpirationSetter images.ExpirationTimeSetter
	)

	s := &Server{
		Server:       baseServer,
		tcpServer:    grpc.NewServer(),
		tapProvider:  new(tapProvider),
		httpHandlers: make(map[string]http.Handler),
		config:       config,
		stopCh:       make(chan struct{}),
	}

	dynamConf.AppendConfigWatcher(s)

	plugins := serverPlugins(baseServer)
	var criticalPluginErrs []string
	for _, p := range plugins {
		instance, err := p.Instance()
		if err != nil {
			id := p.Registration.URI()
			if isCriticalCubeletPlugin(id) {
				criticalPluginErrs = append(criticalPluginErrs, fmt.Sprintf("%s: %v", id, err))
			}
			continue
		}

		id := p.Registration.URI()
		reqID := id
		if config.Config.Version == 1 {
			reqID = p.Registration.ID
		}

		if reqID == constants.CubeboxServicePlugin.String()+"."+constants.MetricID.ID() {
			metricHandler, ok := instance.(http.Handler)
			if !ok {
				return nil, fmt.Errorf("metric plugin have not implemented http.Handler interface")
			}
			s.metricServer = metricHandler
		}
		if reqID == constants.InternalPlugin.String()+"."+resourcemetrics.PluginID {
			resourceMetricHandler, ok := instance.(http.Handler)
			if !ok {
				return nil, fmt.Errorf("resource metrics plugin has not implemented http.Handler")
			}
			s.resourceMetricServer = resourceMetricHandler
		}

		if e, ok := instance.(*workflow.Engine); ok {
			engine = e
		}

		if ies, ok := instance.(images.ExpirationTimeSetter); ok {
			imgExpirationSetter = ies
		}

		if service, ok := instance.(operationService); ok {
			operationServices = append(operationServices, service)
		}
		if service, ok := instance.(httpService); ok {
			httpServices = append(httpServices, service)
		}
		if service, ok := instance.(tcpService); ok {
			tcpServices = append(tcpServices, service)
		}
	}
	if len(criticalPluginErrs) > 0 {
		return nil, fmt.Errorf("critical cubelet plugins failed to initialize: %s", strings.Join(criticalPluginErrs, "; "))
	}

	s.operationServer = NewOperationServer(engine, imgExpirationSetter)

	operationMux := s.operationServer.srv.Handler.(*http.ServeMux)
	for _, service := range operationServices {
		if err := service.RegisterOperation(operationMux); err != nil {
			return nil, err
		}
	}
	for _, service := range httpServices {
		if err := service.RegisterHTTP(s.httpHandlers); err != nil {
			return nil, err
		}
	}
	for _, service := range tcpServices {
		if err := service.RegisterTCP(s.tcpServer); err != nil {
			return nil, err
		}
	}

	return s, nil
}

func isCriticalCubeletPlugin(id string) bool {
	return strings.HasPrefix(id, "io.cubelet.")
}

type Server struct {
	*containerdserver.Server
	operationServer      *OperationServer
	tcpServer            *grpc.Server
	tapProvider          *tapProvider
	metricServer         http.Handler
	resourceMetricServer http.Handler
	httpHandlers         map[string]http.Handler
	config               *srvconfig.Config
	stopCh               chan struct{}
}

func (s *Server) ServeOperation(l net.Listener) error {
	return trapClosedConnErr(s.operationServer.Serve(l))
}

func (s *Server) ServeTap(l net.Listener) error {
	return trapClosedConnErr(s.tapProvider.Serve(l))
}

func (s *Server) ServeGRPC(l net.Listener) error {
	return s.Server.ServeGRPC(l)
}

func (s *Server) ServeTTRPC(l net.Listener) error {
	return s.Server.ServeTTRPC(l)
}

func (s *Server) serveMetrics() map[string]http.Handler {
	handlers := make(map[string]http.Handler)
	handlers["/v1/metrics"] = metrics.Handler()

	if s.metricServer != nil {
		handlers["/v1/metrics/scheduler"] = s.metricServer
	}
	if s.resourceMetricServer != nil {
		handlers["/v1/metrics/resource"] = s.resourceMetricServer
	}

	return handlers
}

func (s *Server) ServeTCP(l net.Listener) error {
	if s.tcpServer == nil {
		return s.Server.ServeTCP(l)
	}
	return trapClosedConnErr(s.tcpServer.Serve(l))
}

// appendHttpHandlers merges HTTP handler maps while rejecting duplicate paths.
// Failing fast here prevents two plugins from silently shadowing the same Cubelet endpoint.
func appendHttpHandlers(dst map[string]http.Handler, src map[string]http.Handler) (map[string]http.Handler, error) {
	for path, handler := range src {
		if dst[path] != nil {
			return nil, fmt.Errorf("duplicate handler for path %q", path)
		}
		dst[path] = handler
	}
	return dst, nil
}

func (s *Server) ServeHttp(l net.Listener) error {

	gin.SetMode(gin.ReleaseMode)

	r := gin.New()

	r.Use(gin.Recovery())

	handlers := map[string]http.Handler{}

	handlers, err := appendHttpHandlers(handlers, s.serveMetrics())

	if err != nil {
		return fmt.Errorf("failed to append http handlers for serveMetrics: %s", err.Error())
	}
	log.G(context.Background()).Infof("ServeHttp: Serve metrics")

	handlers, err = appendHttpHandlers(handlers, serveSNHost())
	if err != nil {
		return fmt.Errorf("failed to append http handlers for serveSNHost: %s", err.Error())
	}

	log.G(context.Background()).Infof("ServeHttp: Serve snhost.")

	// Plugin HTTP handlers are appended after built-in endpoints so path conflicts
	// are detected against the complete Cubelet HTTP surface.
	handlers, err = appendHttpHandlers(handlers, s.httpHandlers)
	if err != nil {
		return fmt.Errorf("failed to append plugin http handlers: %s", err.Error())
	}

	for path, handler := range handlers {

		r.Any(path, func(c *gin.Context) {
			handler.ServeHTTP(c.Writer, c.Request)
		})
	}

	return trapClosedConnErr(http.Serve(l, r))
}

func (s *Server) ServeDebug(l net.Listener) error {

	m := http.NewServeMux()
	m.Handle("/debug/vars", expvar.Handler())
	m.Handle("/debug/pprof/", http.HandlerFunc(pprof.Index))
	m.Handle("/debug/pprof/cmdline", http.HandlerFunc(pprof.Cmdline))
	m.Handle("/debug/pprof/profile", http.HandlerFunc(pprof.Profile))
	m.Handle("/debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
	m.Handle("/debug/pprof/trace", http.HandlerFunc(pprof.Trace))
	m.Handle("/debug/loglevel", http.HandlerFunc(setLogLevel))
	return trapClosedConnErr(http.Serve(l, m))
}

func (s *Server) OnEvent(conf *dynamConf.Config) {
	if conf.Common != nil && conf.Common.LogLevel != "" {
		CubeLog.SetLevel(CubeLog.StringToLevel(strings.ToUpper(conf.Common.LogLevel)))
	}
	if s.operationServer != nil {
		if err := s.operationServer.ApplyHostConfig(); err != nil {
			CubeLog.Errorf("apply host config on config change failed: %v", err)
		}
	}
}

func setLogLevel(w http.ResponseWriter, r *http.Request) {
	l := r.FormValue("level")
	if l == "" {
		return
	}
	CubeLog.SetLevel(CubeLog.StringToLevel(strings.ToUpper(l)))
	lvl, err := logrus.ParseLevel(l)
	if err != nil {
		return
	}
	logrus.SetLevel(lvl)
}

func (s *Server) Stop() {
	ppid := os.Getpid()
	log.L.Errorf("cubelet server stopped gracefully begin, pid %v", ppid)
	CubeLog.WithContext(context.Background()).Errorf("cubelet server stopped gracefully begin, pid %v", ppid)

	graceFulJobs := []func(){
		func() {
			if s.operationServer != nil {
				s.operationServer.Stop()
			}
		},
		func() {
			if s.Server != nil {
				s.Server.Stop()
			}
		},
		func() {
			if s.tcpServer != nil {
				s.tcpServer.Stop()
			}
		},

		func() {
			if s.stopCh != nil {
				close(s.stopCh)
			}
		},
	}

	wg := sync.WaitGroup{}
	wg.Add(len(graceFulJobs))
	notDoneCnt := int32(len(graceFulJobs))

	for _, job := range graceFulJobs {
		go func(j func()) {
			defer wg.Done()
			defer atomic.AddInt32(&notDoneCnt, -1)
			defer utils.Recover()
			j()
		}(job)
	}

	wg.Wait()

	log.L.Errorf("cubelet server stopped gracefully successfully, pid %v", ppid)
	CubeLog.WithContext(context.Background()).Errorf("cubelet server stopped gracefully successfully, pid %v", ppid)
}

func trapClosedConnErr(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "use of closed network connection") {
		return nil
	}
	return err
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"syscall"
	"time"

	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
	"github.com/tencentcloud/CubeSandbox/network-agent/internal/fdserver"
	"github.com/tencentcloud/CubeSandbox/network-agent/internal/grpcserver"
	"github.com/tencentcloud/CubeSandbox/network-agent/internal/httpserver"
	"github.com/tencentcloud/CubeSandbox/network-agent/internal/service"
	"github.com/tencentcloud/CubeSandbox/network-agent/pkg/version"
)

var newLocalService = service.NewLocalService

func main() {
	defaultCfg := service.DefaultConfig()
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "show version information")
	flag.BoolVar(&showVersion, "v", false, "show version information")
	var (
		listenEndpoint = flag.String("listen", "unix:///tmp/cube/network-agent.sock", "network-agent listen endpoint")
		healthListen   = flag.String("health-listen", "127.0.0.1:19090", "network-agent health server listen address")
		grpcListen     = flag.String("grpc-listen", "unix:///tmp/cube/network-agent-grpc.sock", "optional gRPC listen endpoint, supports unix:// and tcp://")
		cubeletConfig  = flag.String("cubelet-config", "", "optional Cubelet config.toml path used to sync network defaults")
		ethName        = flag.String("eth-name", "", "node uplink interface name")
		cidr           = flag.String("cidr", "192.168.0.0/18", "tap sandbox cidr")
		mvmInnerIP     = flag.String("mvm-inner-ip", "169.254.68.6", "guest visible IP inside MVM")
		mvmMacAddr     = flag.String("mvm-mac-addr", "20:90:6f:fc:fc:fc", "guest MAC address")
		mvmGwDestIP    = flag.String("mvm-gw-dest-ip", "169.254.68.5", "guest gateway destination IP")
		mvmGwMacAddr   = flag.String("mvm-gw-mac-addr", "20:90:6f:cf:cf:cf", "guest gateway MAC address")
		mvmMask        = flag.Int("mvm-mask", 30, "guest mask bits")
		mvmMTU         = flag.Int("mvm-mtu", defaultCfg.MvmMtu, "guest mtu")
		stateDir       = flag.String("state-dir", defaultCfg.StateDir, "network-agent state directory")
		tapFDListen    = flag.String("tap-fd-listen", "unix:///tmp/cube/network-agent-tap.sock", "unix socket for passing original tap fds to cubelet")
		hostProxyBind  = flag.String("host-proxy-bind-ip", "127.0.0.1", "host proxy bind ip")
		logPath        = flag.String("logpath", defaultLogDir, "network-agent log directory")
		logLevel       = flag.String("log-level", defaultLogLevel, "set the logging level [debug, info, warn, error, fatal]")
		logRollNum     = flag.Int("log-roll-num", defaultRollNum, "network-agent log files roll number")
		logRollSize    = flag.Int("log-roll-size", defaultRollSizeMB, "network-agent log files roll size(MB)")
		pprofListen    = flag.String("pprof-listen", "", "optional pprof/debug http listen address (e.g. 127.0.0.1:6060); empty disables profiling")
		// Route-aware egress options.
		cubeRouterEnable = flag.Bool("cube-router-enable", defaultCfg.CubeRouterEnable, "enable cube-router route-aware egress")
		cubeRouterCIDR   = flag.String("cube-router-cidr", defaultCfg.CubeRouterCIDR, "optional cube-router IPv4 CIDR; empty derives addresses from sandbox CIDR")
		cubeRouterMAC    = flag.String("cube-router-mac-addr", defaultCfg.CubeRouterMacAddr, "cube-router MAC address")
	)
	flag.Parse()
	if showVersion {
		fmt.Println(version.String())
		os.Exit(0)
	}
	if err := initLogger(*logPath, *logLevel, *logRollNum, *logRollSize); err != nil {
		CubeLog.Fatalf("network-agent init logger failed: %v", err)
		panic(err)
	}

	if *pprofListen != "" {
		startProfilingServer(*pprofListen)
	}

	cfg := defaultCfg
	if *cubeletConfig != "" {
		var err error
		cfg, err = service.LoadConfigFromCubeletTOML(cfg, *cubeletConfig)
		if err != nil {
			CubeLog.Fatalf("network-agent load cubelet config failed: %v", err)
			panic(err)
		}
	}

	overrides := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) {
		overrides[f.Name] = true
	})

	if overrides["eth-name"] {
		cfg.EthName = *ethName
	}
	if overrides["cidr"] {
		cfg.CIDR = *cidr
	}
	if overrides["mvm-inner-ip"] {
		cfg.MVMInnerIP = *mvmInnerIP
	}
	if overrides["mvm-mac-addr"] {
		cfg.MVMMacAddr = *mvmMacAddr
	}
	if overrides["mvm-gw-dest-ip"] {
		cfg.MvmGwDestIP = *mvmGwDestIP
	}
	if overrides["mvm-gw-mac-addr"] {
		cfg.MvmGwMacAddr = *mvmGwMacAddr
	}
	if overrides["mvm-mask"] {
		cfg.MvmMask = *mvmMask
	}
	if overrides["mvm-mtu"] {
		cfg.MvmMtu = *mvmMTU
	}
	if overrides["state-dir"] {
		cfg.StateDir = *stateDir
	}
	if overrides["tap-fd-listen"] {
		cfg.TapFDSocketPath = *tapFDListen
	}
	if overrides["host-proxy-bind-ip"] {
		cfg.HostProxyBindIP = *hostProxyBind
	}
	if overrides["cube-router-enable"] {
		cfg.CubeRouterEnable = *cubeRouterEnable
	}
	if overrides["cube-router-cidr"] {
		cfg.CubeRouterCIDR = *cubeRouterCIDR
	}
	if overrides["cube-router-mac-addr"] {
		cfg.CubeRouterMacAddr = *cubeRouterMAC
	}

	CubeLog.Infof("network-agent startup config: cubelet-config=%q, config={%s}", *cubeletConfig, summarizeConfig(cfg))

	svc, err := initService(cfg)
	if err != nil {
		CubeLog.Fatalf("network-agent init failed: %v", err)
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	CubeLog.Infof("network-agent bootstrap health check with service: %s", describeService(svc))
	if err := svc.Health(ctx); err != nil {
		CubeLog.Fatalf("network-agent bootstrap health check failed: %v", err)
		panic(err)
	}

	apiServer, err := httpserver.NewEndpoint(*listenEndpoint, svc)
	if err != nil {
		CubeLog.Fatalf("network-agent api server init failed: %v", err)
		panic(err)
	}
	go func() {
		if err := apiServer.Start(); err != nil {
			CubeLog.Fatalf("network-agent api server failed: %v", err)
			panic(err)
		}
	}()

	var grpcSrv *grpcserver.Server
	if *grpcListen != "" {
		grpcSrv, err = grpcserver.New(*grpcListen, svc)
		if err != nil {
			CubeLog.Fatalf("network-agent grpc server init failed: %v", err)
			panic(err)
		}
		go func() {
			if err := grpcSrv.Start(); err != nil {
				CubeLog.Fatalf("network-agent grpc server failed: %v", err)
				panic(err)
			}
		}()
	}

	var tapFDSrv *fdserver.Server
	if provider, ok := svc.(service.TapFDProvider); ok && cfg.TapFDSocketPath != "" {
		tapFDSrv, err = fdserver.New(cfg.TapFDSocketPath, provider)
		if err != nil {
			CubeLog.Fatalf("network-agent tap fd server init failed: %v", err)
			panic(err)
		}
		go func() {
			if err := tapFDSrv.Start(); err != nil {
				CubeLog.Fatalf("network-agent tap fd server failed: %v", err)
				panic(err)
			}
		}()
	}

	healthServer := httpserver.New(*healthListen, svc)
	go func() {
		if err := healthServer.Start(); err != nil {
			CubeLog.Fatalf("network-agent health server failed: %v", err)
			panic(err)
		}
	}()

	CubeLog.Infof("network-agent started, listen=%s, grpc-listen=%s, tap-fd-listen=%s, health-listen=%s, cubelet-config=%s",
		*listenEndpoint, *grpcListen, cfg.TapFDSocketPath, *healthListen, *cubeletConfig)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := apiServer.Stop(stopCtx); err != nil {
		CubeLog.Warnf("network-agent api server shutdown error: %v", err)
	}
	if grpcSrv != nil {
		if err := grpcSrv.Stop(stopCtx); err != nil {
			CubeLog.Warnf("network-agent grpc server shutdown error: %v", err)
		}
	}
	if tapFDSrv != nil {
		if err := tapFDSrv.Stop(stopCtx); err != nil {
			CubeLog.Warnf("network-agent tap fd server shutdown error: %v", err)
		}
	}
	if err := healthServer.Stop(stopCtx); err != nil {
		CubeLog.Warnf("network-agent health server shutdown error: %v", err)
	}
}

// startProfilingServer enables the Go pprof endpoints (CPU, heap, goroutine,
// and crucially mutex/block contention) on the given address so the create hot
// path can be profiled under load. Disabled by default; only started when
// --pprof-listen is provided. Mutex/block profiling lets us confirm whether any
// Go-level lock (e.g. localService.mu) is actually contended versus the cost
// living in kernel-serialized netlink/eBPF syscalls.
func startProfilingServer(addr string) {
	runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(1)
	go func() {
		server := &http.Server{Addr: addr, ReadHeaderTimeout: 5 * time.Second}
		CubeLog.Infof("network-agent pprof server listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			CubeLog.Warnf("network-agent pprof server failed: %v", err)
		}
	}()
}

func initService(cfg service.Config) (service.Service, error) {
	CubeLog.Infof("network-agent initService start: config={%s}", summarizeConfig(cfg))
	svc, err := newLocalService(cfg)
	CubeLog.Infof("network-agent initService factory result: service={%s}, err=%v", describeService(svc), err)
	if err != nil {
		return nil, err
	}
	if err := validateService(svc); err != nil {
		CubeLog.Errorf("network-agent validateService failed: service={%s}, err=%v", describeService(svc), err)
		return nil, err
	}
	CubeLog.Infof("network-agent initService success: service={%s}", describeService(svc))
	return svc, nil
}

func validateService(svc service.Service) error {
	if svc == nil {
		return errors.New("network-agent init returned nil service")
	}
	v := reflect.ValueOf(svc)
	if v.Kind() == reflect.Ptr && v.IsNil() {
		return errors.New("network-agent init returned typed nil service")
	}
	return nil
}

func summarizeConfig(cfg service.Config) string {
	return fmt.Sprintf(
		"eth_name=%q object_dir=%q cidr=%q mvm_inner_ip=%q mvm_mac_addr=%q mvm_gw_dest_ip=%q mvm_gw_mac_addr=%q mvm_mask=%d mvm_mtu=%d cube_router_enable=%v cube_router_cidr=%q cube_router_mac_addr=%q tap_init_num=%d state_dir=%q tap_fd_socket_path=%q host_proxy_bind_ip=%q connect_timeout=%s",
		cfg.EthName,
		cfg.ObjectDir,
		cfg.CIDR,
		cfg.MVMInnerIP,
		cfg.MVMMacAddr,
		cfg.MvmGwDestIP,
		cfg.MvmGwMacAddr,
		cfg.MvmMask,
		cfg.MvmMtu,
		cfg.CubeRouterEnable,
		cfg.CubeRouterCIDR,
		cfg.CubeRouterMacAddr,
		cfg.TapInitNum,
		cfg.StateDir,
		cfg.TapFDSocketPath,
		cfg.HostProxyBindIP,
		cfg.ConnectTimeout,
	)
}

func describeService(svc service.Service) string {
	if svc == nil {
		return "nil interface"
	}

	v := reflect.ValueOf(svc)
	desc := fmt.Sprintf("type=%T", svc)
	if !v.IsValid() {
		return desc + ", valid=false"
	}

	desc += fmt.Sprintf(", kind=%s", v.Kind())
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		desc += fmt.Sprintf(", is_nil=%t", v.IsNil())
	}
	if v.Kind() == reflect.Ptr && !v.IsNil() {
		desc += fmt.Sprintf(", ptr=%#x", v.Pointer())
	}

	return desc
}

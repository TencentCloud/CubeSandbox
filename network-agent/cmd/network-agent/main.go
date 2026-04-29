// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"syscall"
	"time"

	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
	"github.com/tencentcloud/CubeSandbox/network-agent/internal/fdserver"
	"github.com/tencentcloud/CubeSandbox/network-agent/internal/grpcserver"
	"github.com/tencentcloud/CubeSandbox/network-agent/internal/httpserver"
	"github.com/tencentcloud/CubeSandbox/network-agent/internal/service"
)

var newLocalService = service.NewLocalService

func main() {
	defaultCfg := service.DefaultConfig()
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
		mvmMTU         = flag.Int("mvm-mtu", 1300, "guest mtu")
		stateDir       = flag.String("state-dir", defaultCfg.StateDir, "network-agent state directory")
		tapFDListen    = flag.String("tap-fd-listen", "unix:///tmp/cube/network-agent-tap.sock", "unix socket for passing original tap fds to cubelet")
		hostProxyBind  = flag.String("host-proxy-bind-ip", "127.0.0.1", "host proxy bind ip")
		logPath        = flag.String("logpath", defaultLogDir, "network-agent log directory")
		logLevel       = flag.String("log-level", defaultLogLevel, "set the logging level [debug, info, warn, error, fatal]")
		logRollNum     = flag.Int("log-roll-num", defaultRollNum, "network-agent log files roll number")
		logRollSize    = flag.Int("log-roll-size", defaultRollSizeMB, "network-agent log files roll size(MB)")
	)
	flag.Parse()
	if err := initLogger(*logPath, *logLevel, *logRollNum, *logRollSize); err != nil {
		CubeLog.Fatalf("network-agent init logger failed: %v", err)
	}

	cfg := defaultCfg
	if *cubeletConfig != "" {
		var err error
		cfg, err = service.LoadConfigFromCubeletTOML(cfg, *cubeletConfig)
		if err != nil {
			CubeLog.Fatalf("network-agent load cubelet config failed: %v", err)
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

	CubeLog.Infof("network-agent startup config: cubelet-config=%q, config={%s}", *cubeletConfig, summarizeConfig(cfg))

	svc, err := initService(cfg)
	if err != nil {
		CubeLog.Fatalf("network-agent init failed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	CubeLog.Infof("network-agent bootstrap health check with service: %s", describeService(svc))
	if err := svc.Health(ctx); err != nil {
		CubeLog.Fatalf("network-agent bootstrap health check failed: %v", err)
	}

	apiServer, err := httpserver.NewEndpoint(*listenEndpoint, svc)
	if err != nil {
		CubeLog.Fatalf("network-agent api server init failed: %v", err)
	}
	go func() {
		if err := apiServer.Start(); err != nil {
			CubeLog.Fatalf("network-agent api server failed: %v", err)
		}
	}()

	var grpcSrv *grpcserver.Server
	if *grpcListen != "" {
		grpcSrv, err = grpcserver.New(*grpcListen, svc)
		if err != nil {
			CubeLog.Fatalf("network-agent grpc server init failed: %v", err)
		}
		go func() {
			if err := grpcSrv.Start(); err != nil {
				CubeLog.Fatalf("network-agent grpc server failed: %v", err)
			}
		}()
	}

	var tapFDSrv *fdserver.Server
	if provider, ok := svc.(service.TapFDProvider); ok && cfg.TapFDSocketPath != "" {
		tapFDSrv, err = fdserver.New(cfg.TapFDSocketPath, provider)
		if err != nil {
			CubeLog.Fatalf("network-agent tap fd server init failed: %v", err)
		}
		go func() {
			if err := tapFDSrv.Start(); err != nil {
				CubeLog.Fatalf("network-agent tap fd server failed: %v", err)
			}
		}()
	}

	healthServer := httpserver.New(*healthListen, svc)
	go func() {
		if err := healthServer.Start(); err != nil {
			CubeLog.Fatalf("network-agent health server failed: %v", err)
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
		"eth_name=%q object_dir=%q cidr=%q mvm_inner_ip=%q mvm_mac_addr=%q mvm_gw_dest_ip=%q mvm_gw_mac_addr=%q mvm_mask=%d mvm_mtu=%d tap_init_num=%d default_exposed_ports=%v state_dir=%q tap_fd_socket_path=%q host_proxy_bind_ip=%q connect_timeout=%s",
		cfg.EthName,
		cfg.ObjectDir,
		cfg.CIDR,
		cfg.MVMInnerIP,
		cfg.MVMMacAddr,
		cfg.MvmGwDestIP,
		cfg.MvmGwMacAddr,
		cfg.MvmMask,
		cfg.MvmMtu,
		cfg.TapInitNum,
		cfg.DefaultExposedPorts,
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

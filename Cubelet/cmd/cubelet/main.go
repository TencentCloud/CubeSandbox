// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
	Copyright (c) 2022 Tencent
	All rights reserved
	Date: 2022-09-22
*/

package main

import (
	gocontext "context"
	"fmt"
	"io"
	stdlog "log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	containerdlog "github.com/containerd/log"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/sys"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/moby/sys/mountinfo"
	"github.com/sirupsen/logrus"
	dynamConf "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/nsenter"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/version"
	"github.com/tencentcloud/CubeSandbox/Cubelet/services/server"
	srvconfig "github.com/tencentcloud/CubeSandbox/Cubelet/services/server/config"
	"github.com/tencentcloud/CubeSandbox/cubelog"
	cubelog "github.com/tencentcloud/CubeSandbox/cubelog"
	"github.com/urfave/cli/v2"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/grpclog"
)

const (
	CubeShimTopdir        = "/run/cube-containers/shared"
	CubeShimSandboxes     = "/run/cube-containers/shared/sandboxes"
	CubeMntNsDirPath      = "/usr/local/services/cubetoolbox/cubeletmnt"
	CubeMntNsFilePath     = "/usr/local/services/cubetoolbox/cubeletmnt/mnt"
	CubeMainProcMutexLock = "/run/cubelock.db"
	networkPluginKey      = "io.cubelet.internal.v1.network"
)

type networkPluginBootstrapConfig struct {
	EnableNetworkAgent   bool   `toml:"enable_network_agent"`
	NetworkAgentEndpoint string `toml:"network_agent_endpoint"`
}

func main() {
	if len(os.Args) > 1 {
		if os.Args[1] == "--version" || os.Args[1] == "-v" || os.Args[1] == "--help" || os.Args[1] == "-h" {
			app := App()
			app.Action = nil
			if err := app.Run(os.Args); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "Cubelet: %s\n", err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}

	os.Setenv("CONTAINERD_SUPPRESS_DEPRECATION_WARNINGS", "true")
	mntPath := os.Getenv("NEED_SET_MNT")

	if mntPath == "" {
		runtime.LockOSThread()
		if needNewMnt(CubeMntNsFilePath) {
			err := newCubeMnt()
			if err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "newCubeMnt fail,%v\n", err)
				time.Sleep(time.Second)
				os.Exit(1)
			}
		}

		err := startSelf(os.Args[1:], []string{fmt.Sprintf("NEED_SET_MNT=%s", CubeMntNsFilePath)})
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "startSelf fail,%v\n", err)
			time.Sleep(time.Second)
			os.Exit(1)
		}

	} else {

		parentExit()
		app := App()
		if err := app.Run(os.Args); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Cubelet: %s\n", err)
			os.Exit(1)
		}
	}

}

func needNewMnt(path string) bool {

	exist, _ := utils.DenExist(path)
	if !exist {
		return true
	}

	exist, _ = mountinfo.Mounted(path)

	return !exist
}

func parentExit() {
	ppid, pid := os.Getppid(), os.Getpid()
	if pp, err := os.FindProcess(ppid); err == nil {
		if err = pp.Kill(); err != nil {
			stdlog.Printf("%d kill parent:%d,err:%v", pid, ppid, err)
		}
	}
}

func startSelf(args []string, env []string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func newCubeMnt() error {

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	newMntPID := 0
	var newMntErr error
	startDone := make(chan int, 1)

	go func(ctx context.Context) {
		defer cancel()
		cmd := exec.CommandContext(ctx, "sleep", "60")
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		// Use Cloneflags only (not Unshareflags) so that Go does not run
		// its automatic post-unshare mount("none", "/", MS_REC|MS_PRIVATE)
		// (see runtime.forkAndExecInChild1 in the Go stdlib). CLONE_NEWNS
		// in Cloneflags still gives us a fresh mount namespace, and the
		// kernel preserves the shared-propagation peer group relationship
		// for mounts copied from the parent. That is a prerequisite for
		// the subsequent `mount --make-rslave /` to slave this ns's root
		// back to the host's shared group.
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
		}
		if err := cmd.Start(); err != nil {
			newMntErr = fmt.Errorf("start newCubeMnt job error: %v", err)
			return
		}
		newMntPID = cmd.Process.Pid
		startDone <- 1
		if err := cmd.Wait(); err != nil {
			newMntErr = fmt.Errorf("wait newCubeMnt job error: %v", err)
			return
		}

	}(ctx)
	<-startDone
	if newMntPID == 0 || newMntErr != nil {
		return fmt.Errorf("new mnt job fail:%v", newMntErr)
	}

	_ = unix.Unmount(CubeShimTopdir, 0)
	_ = unix.Unmount(CubeMntNsDirPath, 0)
	_ = unix.Unmount(CubeMntNsFilePath, 0)

	cmds := [][]string{
		{"mkdir", "-p", CubeShimTopdir},
		{"mkdir", "-p", CubeShimSandboxes},
		// Re-slave the root of cubelet's new mount namespace so that mount
		// events happening in the host (init) namespace — e.g. operators
		// mounting a new network storage volume after cubelet has started —
		// propagate into this ns and become visible to prepareHostDirVolume.
		// Without this, a freshly-added host mount is invisible here and
		// any sandbox requesting a hostPath under it fails with ENOENT.
		// Propagation is one-way (slave): mounts performed inside this ns
		// (shim bind-mounts etc.) do not flow back to the host ns.
		// Prerequisite: the host's "/" must be shared (true on systemd-based
		// distros by default; our compute nodes confirm shared:1).
		{"nsenter", "-t", fmt.Sprintf("%d", newMntPID), "-m", "mount", "--make-rslave", "/"},
		{"nsenter", "-t", fmt.Sprintf("%d", newMntPID), "-m", "mount", "--bind", "--make-shared", CubeShimTopdir, CubeShimTopdir},
		{"mkdir", "-p", CubeMntNsDirPath},
		{"touch", CubeMntNsFilePath},
		{"mount", "--bind", "--make-private", CubeMntNsDirPath, CubeMntNsDirPath},
		{"mount", "--bind", fmt.Sprintf("/proc/%d/ns/mnt", newMntPID), CubeMntNsFilePath},
		{"kill", "-9", fmt.Sprintf("%d", newMntPID)},
	}

	for _, cmd := range cmds {
		if _, stderr, err := utils.ExecV(cmd, utils.DefaultTimeout); err != nil {
			return fmt.Errorf("stable mnt mountpoint err:%v,%v:%v", cmd, stderr, err)
		}
	}
	return nil
}

const usage = `cubelet --help`

func init() {

	grpclog.SetLoggerV2(grpclog.NewLoggerV2(io.Discard, io.Discard, io.Discard))

	cli.VersionPrinter = func(c *cli.Context) {
		fmt.Println(c.App.Name, c.App.Version)
	}
}

func App() *cli.App {
	app := cli.NewApp()
	app.Name = "cubelet"
	app.Version = version.Version
	app.Usage = usage
	app.Description = `cubelet xxxx`
	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:    "config",
			Aliases: []string{"c"},
			Usage:   "path to the configuration file",
			Value:   filepath.Join(defaults.DefaultConfigDir, "config.toml"),
		},
		&cli.StringFlag{
			Name:    "log-level",
			Aliases: []string{"l"},
			Value:   "warn",
			Usage:   "set the logging level [trace, debug, info, warn, error, fatal, panic]",
		},
		&cli.StringFlag{
			Name:    "address",
			Aliases: []string{"a"},
			Usage:   "address for cubelet's GRPC server",
		},
		&cli.StringFlag{
			Name:  "root",
			Usage: "cubelet root directory",
		},
		&cli.StringFlag{
			Name:  "state",
			Usage: "cubelet state directory",
		},
		&cli.StringFlag{
			Name:  "logpath",
			Value: "/data/log/Cubelet",
			Usage: "cubelog log directory",
		},
		&cli.IntFlag{
			Name:  "log-roll-num",
			Value: 10,
			Usage: "cubelog files roll number",
		},
		&cli.IntFlag{
			Name:  "log-roll-size",
			Value: 500,
			Usage: "cubelog files roll size(MB)",
		},
		&cli.IntFlag{
			Name:  "state-tmpfs-size",
			Value: 500,
			Usage: "state-tmpfs-size(MB)",
		},
		&cli.IntFlag{
			Name:  "go-max-procs",
			Value: 32,
		},
		&cli.IntFlag{
			Name:  "go-gc-percent",
			Value: 500,
		},
		&cli.StringFlag{
			Name:  "dynamic-conf-path",
			Value: "/usr/local/services/cubetoolbox/Cubelet/dynamicconf/conf.yaml",
			Usage: "dynamic config path for cubelet",
		},
		&cli.BoolFlag{
			Name:  "no-dynamic-path",
			Usage: "start with no dynamic path",
		},
	}
	app.Action = func(context *cli.Context) error {
		var (
			start         = time.Now()
			signals       = make(chan os.Signal, 2048)
			serverC       = make(chan *server.Server, 1)
			ctx, cancel   = gocontext.WithCancel(gocontext.Background())
			config        = platformAgnosticDefaultConfig()
			mainMutexLock *utils.CubeStore
		)

		runtime.GOMAXPROCS(context.Int("go-max-procs"))
		debug.SetGCPercent(context.Int("go-gc-percent"))

		defer cancel()

		configPath := context.String("config")
		_, err := os.Stat(configPath)
		if !os.IsNotExist(err) || context.IsSet("config") {
			if err := srvconfig.LoadConfig(ctx, configPath, config); err != nil {
				return err
			}
		}

		if err := applyFlags(context, config); err != nil {
			return err
		}

		if networkCfg, ok, err := loadNetworkPluginBootstrapConfig(config); err != nil {
			return err
		} else if ok {
			dynamConf.SetNetworkAgentOverride(networkCfg.EnableNetworkAgent, networkCfg.NetworkAgentEndpoint)
		}

		_, err = dynamConf.Init(config.DynamicConfigPath, context.Bool("no-dynamic-path"))
		if err != nil {
			return err
		}

		initCubeLog(context, "Cubelet", context.String("logpath"))

		if err := server.CreateTopLevelDirectories(config); err != nil {
			return err
		}

		done := handleSignals(ctx, signals, serverC, cancel)

		signal.Notify(signals, handledSignals...)

		mainMutexLock, err = utils.NewCubeStore(CubeMainProcMutexLock, &bolt.Options{
			Timeout: time.Second,
		})
		if err != nil {
			CubeLog.WithContext(ctx).Fatalf("mainMutexLock fail")
			return fmt.Errorf("mainMutexLock: %w", err)
		}
		defer mainMutexLock.Close()

		err = setPidFile(config.PidFile)
		if err != nil {
			log.G(ctx).WithError(err).Warn("set pid file failed")
		}

		if err := mountTmpfsDir(config.State, context); err != nil {
			return fmt.Errorf("mountTmpfsDir: %w", err)
		}

		if err := ensureRootSharedMount(config.Root); err != nil {
			log.G(ctx).WithError(err).Warn("ensure root shared mount failed")
		}

		if err := mount.SetTempMountLocation(filepath.Join(config.Root, "tmpmounts")); err != nil {
			return fmt.Errorf("creating temp mount location: %w", err)
		}

		warnings, err := mount.CleanupTempMounts(0)
		if err != nil {
			log.G(ctx).WithError(err).Error("unmounting temp mounts")
		}
		for _, w := range warnings {
			log.G(ctx).WithError(w).Warn("cleanup temp mount")
		}

		cubeTapPath := os.Getenv("CUBE_TAP_SERV_PATH")
		if cubeTapPath != "" {
			config.CubeTap.Address = cubeTapPath
		}
		log.G(ctx).Debugf("cubeTap path is %s", config.CubeTap.Address)

		if config.GRPC.Address == "" {
			return fmt.Errorf("grpc address cannot be empty: %w", errdefs.ErrInvalidArgument)
		}
		if config.TTRPC.Address == "" {

			config.TTRPC.Address = fmt.Sprintf("%s.ttrpc", config.GRPC.Address)
			config.TTRPC.UID = config.GRPC.UID
			config.TTRPC.GID = config.GRPC.GID
		}
		log.G(ctx).WithFields(logrus.Fields{
			"version":  version.Version,
			"revision": version.Revision,
		}).Info("starting cubelet")

		type srvResp struct {
			s   *server.Server
			err error
		}

		chsrv := make(chan srvResp)
		go func() {
			defer close(chsrv)

			serverTmp, err := server.New(ctx, config)
			if err != nil {
				select {
				case chsrv <- srvResp{err: err}:
				case <-ctx.Done():
				}
				return
			}

			select {
			case <-ctx.Done():
				serverTmp.Stop()
			case chsrv <- srvResp{s: serverTmp}:
			}
		}()

		var serverTmp *server.Server
		select {
		case <-ctx.Done():
			return ctx.Err()
		case r := <-chsrv:
			if r.err != nil {
				return r.err
			}
			serverTmp = r.s
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case serverC <- serverTmp:
		}

		if config.Debug.Address != "" {
			var l net.Listener
			if isLocalAddress(config.Debug.Address) {
				if l, err = sys.GetLocalListener(config.Debug.Address, config.Debug.UID, config.Debug.GID); err != nil {
					return fmt.Errorf("failed to get listener for debug endpoint: %w", err)
				}
			} else {
				if l, err = net.Listen("tcp", config.Debug.Address); err != nil {
					return fmt.Errorf("failed to get listener for debug endpoint: %w", err)
				}
			}
			serve(ctx, l, serverTmp.ServeDebug)
		}

		if config.HttpConfig.Address != "" {
			l, err := net.Listen("tcp", config.HttpConfig.Address)
			if err != nil {
				return fmt.Errorf("failed to get listener for Http endpoint: %w", err)
			}
			serve(ctx, l, serverTmp.ServeHttp)
		}

		tl, err := sys.GetLocalListener(config.TTRPC.Address, config.TTRPC.UID, config.TTRPC.GID)
		if err != nil {
			return fmt.Errorf("failed to get listener for main ttrpc endpoint: %w", err)
		}
		serve(ctx, tl, serverTmp.ServeTTRPC)

		if config.GRPC.TCPAddress != "" {
			l, err := net.Listen("tcp", config.GRPC.TCPAddress)
			if err != nil {
				return fmt.Errorf("failed to get listener for TCP grpc endpoint: %w", err)
			}
			serve(ctx, l, serverTmp.ServeTCP)
		}

		l, err := sys.GetLocalListener(config.GRPC.Address, config.GRPC.UID, config.GRPC.GID)
		if err != nil {
			return fmt.Errorf("failed to get listener for main endpoint: %w", err)
		}
		serve(ctx, l, serverTmp.ServeGRPC)

		if config.OperationServer.Address != "" && !config.OperationServer.Disable {
			ol, err := sys.GetLocalListener(config.OperationServer.Address, config.OperationServer.UID, config.OperationServer.GID)
			if err != nil {
				return fmt.Errorf("failed to get listener for operation sockPath: %w", err)
			}
			serve(ctx, ol, serverTmp.ServeOperation)
		}

		ul, err := sys.GetLocalListener(config.CubeTap.Address, config.CubeTap.UID, config.CubeTap.GID)
		if err != nil {
			return fmt.Errorf("failed to get listener for cubetap sockPath: %w", err)
		}
		serve(ctx, ul, serverTmp.ServeTap)

		if err := notifyReady(ctx); err != nil {
			log.G(ctx).WithError(err).Warn("notify ready failed")
		}
		log.G(ctx).Infof("cubelet successfully booted in %fs", time.Since(start).Seconds())
		CubeLog.WithContext(ctx).Errorf("cubelet successfully booted in %fs", time.Since(start).Seconds())

		logLevel := dynamConf.GetCommon().LogLevel
		if logLevel == "" {
			logLevel = context.String("log-level")
		}
		cubelog.SetLevel(cubelog.StringToLevel(strings.ToUpper(logLevel)))
		containerdlog.SetLevel(strings.ToLower(logLevel))
		<-done
		return nil
	}
	return app
}

func loadNetworkPluginBootstrapConfig(cfg *srvconfig.Config) (*networkPluginBootstrapConfig, bool, error) {
	if cfg == nil || cfg.Plugins == nil {
		return nil, false, nil
	}
	_, ok := cfg.Plugins[networkPluginKey]
	if !ok {
		return nil, false, nil
	}
	var networkCfg networkPluginBootstrapConfig
	if _, err := cfg.Decode(gocontext.Background(), networkPluginKey, &networkCfg); err != nil {
		return nil, false, fmt.Errorf("decode %s plugin config: %w", networkPluginKey, err)
	}
	return &networkCfg, true, nil
}

func serve(ctx gocontext.Context, l net.Listener, serveFunc func(net.Listener) error) {
	path := l.Addr().String()
	log.G(ctx).WithField("address", path).Info("serving...")
	go func() {
		defer func() { _ = l.Close() }()
		if err := serveFunc(l); err != nil {
			log.G(ctx).WithError(err).WithField("address", path).Fatal("serve failure")
		}
	}()
}

func applyFlags(context *cli.Context, config *srvconfig.Config) error {

	if err := setLogLevel(context, config); err != nil {
		return err
	}

	for _, v := range []struct {
		name string
		d    *string
	}{
		{
			name: "root",
			d:    &config.Root,
		},
		{
			name: "state",
			d:    &config.State,
		},
		{
			name: "address",
			d:    &config.GRPC.Address,
		},
		{
			name: "dynamic-conf-path",
			d:    &config.DynamicConfigPath,
		},
	} {
		if s := context.String(v.name); s != "" {
			*v.d = s
		}
	}
	return nil
}

func setLogLevel(context *cli.Context, config *srvconfig.Config) error {
	l := context.String("log-level")
	if l == "" {
		l = config.Debug.Level
	}
	if l != "" {
		lvl, err := logrus.ParseLevel(l)
		if err != nil {
			return err
		}
		logrus.SetLevel(lvl)
	}
	return nil
}

func ensureRootSharedMount(rootDir string) error {

	if _, err := os.Stat(rootDir); os.IsNotExist(err) {
		return nil
	}

	mountInfo, err := mountinfo.GetMounts(mountinfo.SingleEntryFilter(rootDir))
	if err != nil {
		return fmt.Errorf("failed to get mount info for %s: %w", rootDir, err)
	}

	if len(mountInfo) == 0 {

		if err := unix.Mount(rootDir, rootDir, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("failed to bind mount %s: %w", rootDir, err)
		}

		if err := unix.Mount("", rootDir, "", unix.MS_SHARED, ""); err != nil {
			return fmt.Errorf("failed to make %s shared: %w", rootDir, err)
		}
		stdlog.Printf("set %s as shared mount", rootDir)
		return nil
	}

	for _, info := range mountInfo {
		optsSplit := strings.Split(info.Optional, " ")
		for _, opt := range optsSplit {
			if strings.HasPrefix(opt, "shared:") {
				return nil
			}
		}
	}

	if err := unix.Mount("", rootDir, "", unix.MS_SHARED, ""); err != nil {
		return fmt.Errorf("failed to make %s shared: %w", rootDir, err)
	}
	stdlog.Printf("set %s as shared mount", rootDir)
	return nil
}

func mountTmpfsDir(stateDir string, context *cli.Context) error {
	exist, _ := mountinfo.Mounted(stateDir)
	if exist {
		return nil
	}
	size := context.Int("state-tmpfs-size")
	_ = mount.UnmountAll(stateDir, 0)
	m := &mount.Mount{
		Type:    "tmpfs",
		Source:  "none",
		Options: []string{fmt.Sprintf("size=%dm", size)},
	}
	if err := m.Mount(stateDir); err != nil {
		return err
	}
	exist, _ = mountinfo.Mounted(stateDir)
	if !exist {
		return fmt.Errorf("mount tmpfs:%v fail", stateDir)
	}
	return nil
}
func setPidFile(pidFile string) error {
	if pidFile == "" {
		return nil
	}
	pid := os.Getpid()
	data := strconv.FormatInt(int64(pid), 10)
	return os.WriteFile(pidFile, []byte(data), 0644)
}

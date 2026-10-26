// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package chics

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/runtime"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"

	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cbri"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/chi"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/chi/vsockets"
)

var (
	defaultProxyTimeout         = 60 * time.Second
	defaultCubeHostImageTimeout = 60 * time.Second
)

type CubeHostFactoryConfig struct {
	CubeSocketName string `toml:"cubeSocketName"`
	VMRootDir      string `toml:"vmRootDir"`

	ProxyPort uint32 `toml:"proxyPort"`

	CubeHostImagePort uint32 `toml:"cubeHostImagePort"`
}

func (v *CubeHostFactoryConfig) GetCubeUdsPath(vmID string) string {
	return filepath.Join(v.VMRootDir, vmID, v.CubeSocketName)
}

type cubeHostClientManagerLocal struct {
	config      *CubeHostFactoryConfig
	store       *runtime.NSMap[*cubeHostImageReverseClient]
	cri         cbri.CubeRuntimeImplementation
	hostMonitor *hostImageForwardCollector
}

func NewCubeHostClientManager(config *CubeHostFactoryConfig) (chi.ChiFactory, error) {
	chcl := &cubeHostClientManagerLocal{
		config:      config,
		store:       runtime.NewNSMap[*cubeHostImageReverseClient](),
		hostMonitor: newHostImageForwardCollector(),
	}

	return chcl, nil
}

var _ cbri.APIInit = &cubeHostClientManagerLocal{}

func (v *cubeHostClientManagerLocal) SetCubeRuntimeImplementation(cri cbri.CubeRuntimeImplementation) {
	v.cri = cri
}

func (v *cubeHostClientManagerLocal) RunForwardCubeHostImage(ctx context.Context,
	updater chi.CubeboxRuntimeUpdater,
	cubeBox *cubeboxstore.CubeBox,
	container containerd.Container) error {

	ns, ok := namespaces.Namespace(ctx)
	if !ok {
		ns = namespaces.Default
		ctx = namespaces.WithNamespace(ctx, ns)
	}
	sandboxID := cubeBox.SandboxID

	svc := newCubeHostImageReverseClient(v.newSandboxVSocketConnFactory(sandboxID), updater, ns, container, v.hostMonitor)
	err := v.store.Add(ctx, svc)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			err = v.CloseForwardCubeHostImage(ctx, sandboxID)
			if err != nil {
				return err
			}
			err = v.store.Add(ctx, svc)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	go func() {
		svc.Run()
		v.store.Delete(ctx, sandboxID)
		log.G(ctx).WithField("sandboxID", sandboxID).Info("cube host image reverse server closed")
	}()

	return nil
}

func (v *cubeHostClientManagerLocal) CloseForwardCubeHostImage(ctx context.Context, sandboxID string) error {
	old, err := v.store.Get(ctx, sandboxID)
	if errdefs.IsNotFound(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to get cube host image reverse server: %w", err)
	}
	old.Close()
	v.store.Delete(ctx, sandboxID)
	return nil
}

func (v *cubeHostClientManagerLocal) newSandboxVSocketConnFactory(sandboxID string) *sandboxVSocketFactory {
	return &sandboxVSocketFactory{
		sandboxID: sandboxID,
		udspath:   v.config.GetCubeUdsPath(sandboxID),
		config:    v.config,
	}
}

func (v *cubeHostClientManagerLocal) NewVSocketConn(sandboxID string, port int, timeout time.Duration) (net.Conn, error) {
	addr := v.config.GetCubeUdsPath(sandboxID)
	hybridVSock := vsockets.HybridVSock{
		UdsPath: addr,
		Port:    uint32(port),
	}
	return vsockets.HybridVSockDialer(hybridVSock.String(), timeout)
}

type sandboxVSocketFactory struct {
	sandboxID string
	udspath   string
	config    *CubeHostFactoryConfig
}

func (svs *sandboxVSocketFactory) ID() string {
	return svs.sandboxID
}

func (svs *sandboxVSocketFactory) NewConn(opts vsockets.SandboxVscoketConnOption) (net.Conn, error) {
	var (
		port    uint32
		timeout time.Duration
	)
	switch opts.Type {
	case vsockets.CubeConnCubeHost:
		port = svs.config.CubeHostImagePort
		timeout = defaultCubeHostImageTimeout
	case vsockets.CubeConnHttpProxy:
		port = svs.config.ProxyPort
		timeout = defaultProxyTimeout
	default:
		return nil, fmt.Errorf("unknown cube conn type %s", opts.Type)
	}

	if opts.Timeout != 0 {
		timeout = opts.Timeout
	}

	hybridVSock := vsockets.HybridVSock{
		UdsPath: svs.udspath,
		Port:    port,
	}
	return vsockets.HybridVSockDialer(hybridVSock.String(), timeout)
}

func (svs *sandboxVSocketFactory) CreateCubeHostConn(ctx context.Context) (net.Conn, error) {
	return svs.NewConn(vsockets.SandboxVscoketConnOption{
		Type:    vsockets.CubeConnCubeHost,
		Timeout: defaultCubeHostImageTimeout,
	})
}

func (svs *sandboxVSocketFactory) CreateHttpProxyConn() (net.Conn, error) {
	return svs.NewConn(vsockets.SandboxVscoketConnOption{
		Type:    vsockets.CubeConnHttpProxy,
		Timeout: defaultProxyTimeout,
	})
}

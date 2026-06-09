// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubelet

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate"
	cubeletnodemeta "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet/nodemeta"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet/versioninfo"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/masterclient"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/networkagentclient"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"

	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/server/images"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/tomlext"
)

const (
	nodeReadyGracePeriod = 120 * time.Second

	nodeStatusUpdateRetry = 5
)

type KubeletConfig struct {
	Insecurity     bool          `toml:"insecurity"`
	ResyncInterval time.Duration `toml:"resync_interval,omitempty"`

	DisableCreateNode bool `toml:"disable_create_node,omitempty"`

	NodeStatusUpdateFrequency tomlext.Duration `toml:"node_status_update_frequency,omitempty"`
}

func DefaultCubeletConfig() *KubeletConfig {
	return &KubeletConfig{
		Insecurity:                true,
		ResyncInterval:            10 * time.Hour,
		DisableCreateNode:         false,
		NodeStatusUpdateFrequency: tomlext.FromStdTime(10 * time.Second),
	}
}

type Cubelet struct {
	hostname string

	nodeIPs []net.IP

	nodeName types.NodeName

	kubeClient   any
	masterClient *masterclient.Client

	registerNode bool

	nodeLister any

	NodeHasSynced func() bool

	providerID            string
	externalCloudProvider bool
	instanceType          string

	NodeRef *corev1.ObjectReference

	NodeLabels map[string]string

	nodeStatusUpdateFrequency time.Duration

	nodeStatusReportFrequency time.Duration

	delayAfterNodeStatusChange time.Duration

	updateRuntimeMux sync.Mutex

	lastStatusReportTime time.Time

	containerRuntimeReadyExpected bool

	registrationCompleted bool

	syncNodeStatusMux sync.Mutex

	resyncInterval time.Duration

	SetNodeStatusFuncs []func(context.Context, *cubeletnodemeta.Node) error
	config             *KubeletConfig

	clock clock.WithTicker

	controllerMap map[string]controller.CubeMetaController

	criImage *cubeimages.CubeImageService

	rtManager runtemplate.RunTemplateManager

	networkAgentClient networkagentclient.Client
	lastNodeSnapshot   *cubeletnodemeta.Node

	versionCollector *versioninfo.Collector

	closeCh chan struct{}
}

func NewCubelet(
	mconfig *KubeletConfig,
	client *masterclient.Client,
	controllerMap map[string]controller.CubeMetaController,
	criImage *cubeimages.CubeImageService,
	rtManager runtemplate.RunTemplateManager,
	networkAgentClient networkagentclient.Client,
) (*Cubelet, error) {
	var (
		ctx        = context.Background()
		err        error
		ips        []net.IP
		nodeLabels map[string]string
	)
	identity, err := utils.GetHostIdentity()
	if err != nil {
		return nil, fmt.Errorf("failed to get host identity: %w", err)
	}

	ips = append(ips, net.ParseIP(identity.LocalIPv4))
	nodeLabels = map[string]string{
		corev1.LabelHostname:        identity.InstanceID,
		corev1.LabelMetadataName:    identity.InstanceID,
		constants.LabelInstanceType: identity.InstanceType,
	}

	clet := &Cubelet{
		hostname:                  identity.InstanceID,
		nodeName:                  types.NodeName(identity.InstanceID),
		providerID:                identity.InstanceID,
		nodeIPs:                   ips,
		instanceType:              identity.InstanceType,
		masterClient:              client,
		config:                    mconfig,
		registerNode:              !mconfig.DisableCreateNode,
		nodeStatusUpdateFrequency: tomlext.ToStdTime(mconfig.NodeStatusUpdateFrequency),
		nodeStatusReportFrequency: tomlext.ToStdTime(mconfig.NodeStatusUpdateFrequency),

		criImage:           criImage,
		rtManager:          rtManager,
		controllerMap:      controllerMap,
		networkAgentClient: networkAgentClient,

		NodeLabels: nodeLabels,

		clock:            clock.RealClock{},
		versionCollector: versioninfo.NewCollector(""),
		closeCh:          make(chan struct{}),
	}

	clet.NodeHasSynced = func() bool { return true }
	clet.nodeLister = struct{}{}
	if clet.masterClient != nil {
		log.G(ctx).Infof("Attempting to sync node with CubeMaster metadata service")
	} else {
		log.G(ctx).Infof("Cubelet is running without metadata service client, will skip node sync")
	}

	nodeRef := &corev1.ObjectReference{
		Kind:      "Node",
		Name:      string(clet.hostname),
		UID:       types.UID(clet.hostname),
		Namespace: "",
	}
	clet.NodeRef = nodeRef

	clet.SetNodeStatusFuncs = clet.defaultNodeStatusFuncs()
	clet.rtManager.SetInstanceType(clet.instanceType)
	return clet, nil
}

func (kl *Cubelet) Run(readyHook func()) error {
	var (
		err    error
		stopCh = kl.closeCh
	)
	if kl.masterClient == nil {
		klog.InfoS("No CubeMaster metadata service defined - no node status update will be sent")
	}

	if kl.masterClient != nil {

		go func() {

			wait.JitterUntil(kl.syncNodeStatus, kl.nodeStatusUpdateFrequency, 0.04, true, stopCh)
		}()

		go kl.fastStatusUpdateOnce()

	}

	errChan := make(chan error, len(kl.controllerMap))
	for controllerName, c := range kl.controllerMap {
		func(controllerName string, c controller.CubeMetaController) {
			recov.GoWithRecover(func() {
				err = c.Run(stopCh)
				if err != nil {
					errChan <- fmt.Errorf("failed to run controller %s: %w", controllerName, err)
				}
			})
		}(controllerName, c)
	}

	readyHook()

	select {
	case err := <-errChan:
		return err
	case <-stopCh:
		return nil
	}
}

func getMetaConfig() *config.MetaServerConfig {
	cfg := config.GetConfig()
	if cfg == nil {
		return nil
	}
	return cfg.MetaServerConfig
}

var _ io.Closer = &Cubelet{}

func (kl *Cubelet) Close() error {
	close(kl.closeCh)
	return nil
}

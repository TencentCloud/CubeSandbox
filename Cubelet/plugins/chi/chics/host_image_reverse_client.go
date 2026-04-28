// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package chics

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"runtime/debug"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	"encoding/json"
	"encoding/json"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubehost/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/chi"
	"github.com/tencentcloud/CubeSandbox/cubelog"
)

var (
	defaultProxyUrl *url.URL
	DefaultConfig   = backoff.Config{
		BaseDelay:  100 * time.Millisecond,
		Multiplier: 1.6,
		Jitter:     0.2,
		MaxDelay:   3 * time.Second,
	}
	retryInterval = 100 * time.Millisecond
)

func init() {
	vmUrl, err := url.Parse("http://localhost:1028")
	if err != nil {
		panic(err)
	}
	defaultProxyUrl = vmUrl
}

type cubeHostImageReverseClient struct {
	ns      string
	factory *sandboxVSocketFactory

	updater chi.CubeboxRuntimeUpdater

	grpcClient *grpc.ClientConn
	vmClient   cubehost.CubeHostImageServiceClient
	container  containerd.Container

	errChanClosed bool
	errChan       chan error

	lock      *sync.Mutex
	isClosed  bool
	closeChan chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc

	heartBeatTime time.Time

	inflatingRequestMap map[string]*cubehost.ServerMessage
	csm                 *containerStatusManager

	hostMonitor *hostImageForwardCollector
}

func newCubeHostImageReverseClient(factory *sandboxVSocketFactory, updater chi.CubeboxRuntimeUpdater, ns string, container containerd.Container, hostMonitor *hostImageForwardCollector) *cubeHostImageReverseClient {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = namespaces.WithNamespace(ctx, ns)

	s := &cubeHostImageReverseClient{
		factory:             factory,
		closeChan:           make(chan struct{}),
		updater:             updater,
		ns:                  ns,
		container:           container,
		lock:                &sync.Mutex{},
		ctx:                 ctx,
		cancel:              cancel,
		errChan:             make(chan error, 1),
		inflatingRequestMap: make(map[string]*cubehost.ServerMessage),
		hostMonitor:         hostMonitor,
	}
	s.csm = &containerStatusManager{
		container:     s.container,
		s:             s,
		checkInterval: 5 * time.Second,
		lastCheckTime: time.Now(),
		isRunning:     true,
	}
	return s
}

func (s *cubeHostImageReverseClient) ID() string {
	return s.factory.sandboxID
}

func (s *cubeHostImageReverseClient) Run() error {

	var (
		client cubehost.CubeHostImageService_ReverseStreamForwardImageClient
		err    error
	)

	rt := CubeLog.GetTraceInfo(s.ctx)
	if rt == nil {
		rt = &CubeLog.RequestTrace{}
	}
	rt.Action = "CubeUpdater"
	rt.InstanceID = s.factory.sandboxID
	rt.Namespace = s.ns
	rt.FunctionType = "cubebox"
	rt.Caller = "cubeHostImageReverseClient"
	s.ctx = CubeLog.WithRequestTrace(s.ctx, rt)
	stepLog := log.G(s.ctx).WithFields(CubeLog.Fields{
		"udspath":                    s.factory.udspath,
		"containerID":                s.container.ID(),
		string(CubeLog.KeyNamespace): s.ns,
	})
	s.ctx = log.WithLogger(s.ctx, stepLog)

	ctx := s.ctx
	stepLog = log.G(ctx).WithFields(CubeLog.Fields{"step": "cube host image reverse client loop"})
	stepLog.Debug("start cube host image reverse client loop")
	defer func() {
		s.Close()
		if err != nil {
			stepLog.Errorf("loop exited with error: %v", err)
		} else {
			stepLog.Infof("loop exited")
		}
	}()

	if s.csm != nil {
		go s.csm.run()
	}

	for {
		stepLog.WithField("step", "init cube host image reverse client")
		if err != nil {
			time.Sleep(retryInterval)
		}

		if s.updater.GetSandboxer() == nil {
			stepLog.Infof("sandbox %s not found", s.factory.sandboxID)
			return fmt.Errorf("sandbox %s may have been removed", s.factory.sandboxID)
		}

		var grpcClient *grpc.ClientConn
		grpcClient, err = s.createCubeHostClient()
		if err != nil {
			stepLog.Debugf("failed to create vm client, will be retry later: %w", err)
			continue
		}
		stepLog.Debug("cube host client created")
		s.grpcClient = grpcClient
		s.vmClient = cubehost.NewCubeHostImageServiceClient(grpcClient)

		client, err = s.initReverseStreamForwardImageClient(ctx, s.vmClient)
		if err != nil {
			stepLog.Debugf("failed to init reverse stream: %v", err)
			continue
		}
		stepLog.Debug("client init successfully")
		go s.handleVmRequest(ctx, client)

		if !s.isRunningContainer() {
			stepLog.Debugf("container is not running")
			continue
		}
		stepLog.Debugf("cube reverse forward image stream init successfully, start to handle cube host server message")
		select {
		case <-s.closeChan:
			return nil
		case e := <-s.errChan:
			if client != nil {
				client.CloseSend()
			}
			if e != nil {
				log.G(ctx).Errorf("failed to handle cube host server message, it will retry later: %v", e)
				err = e
				continue
			}
		}
	}
}

type containerStatusManager struct {
	container     containerd.Container
	s             *cubeHostImageReverseClient
	checkInterval time.Duration
	lastCheckTime time.Time
	isRunning     bool
	notFoundTimes int
}

func (csm *containerStatusManager) run() {
	ctx := csm.s.ctx
	logEntry := log.G(ctx).WithFields(CubeLog.Fields{
		"step": "container status manager",
	})

	logEntry.Debug("start container status manager")
	defer func() {
		logEntry.Debug("container status manager exited")
	}()

	for {
		select {
		case <-csm.s.closeChan:
			return
		default:
			if time.Since(csm.lastCheckTime) > csm.checkInterval {
				csm.lastCheckTime = time.Now()

				timeCtx, cancel := context.WithTimeout(ctx, csm.checkInterval)
				defer cancel()

				timeCtx = namespaces.WithNamespace(timeCtx, csm.s.ns)
				running := false

				t, err := csm.container.Task(timeCtx, nil)
				if err != nil {
					logEntry.Errorf("container status manager failed to get container status: %v", err)
					if errdefs.IsNotFound(err) {
						csm.notFoundTimes++
						if csm.notFoundTimes > 3 {
							csm.s.Close()
							return
						}
						continue
					}
				} else {
					status, err := t.Status(timeCtx)
					if err != nil {
						log.G(ctx).Errorf("failed to get task status: %w", err)
					} else if status.Status == containerd.Running {
						running = true
					} else {
						logEntry.Errorf("container status is not running: %v", status.Status)
					}
				}
				csm.notFoundTimes = 0

				csm.isRunning = running
			}
			time.Sleep(csm.checkInterval)
		}
	}
}

func (s *cubeHostImageReverseClient) isRunningContainer() bool {
	if s.csm == nil {
		return true
	}
	if s.csm.lastCheckTime == (time.Time{}) {
		return true
	}
	if s.csm.lastCheckTime.Add(3 * s.csm.checkInterval).Before(time.Now()) {
		return false
	}
	return s.csm.isRunning
}

func (s *cubeHostImageReverseClient) handleVmRequest(ctx context.Context, client cubehost.CubeHostImageService_ReverseStreamForwardImageClient) {
	ctx = namespaces.WithNamespace(ctx, s.ns)
	logEntry := log.G(ctx).WithField("step", "handle cube host server message")
	defer recov.HandleCrash(func(panicError interface{}) {
		logEntry.Error("EniManager Create panic info:%s, stack:%s", panicError, string(debug.Stack()))
	})

	for {
		select {
		case <-s.closeChan:
			return
		case <-s.ctx.Done():
			return
		default:
			vmReq, err := client.Recv()
			if err != nil {
				s.sendError(fmt.Errorf("failed to receive message: %w", err))
				return
			}
			s.asyncDispatch(ctx, client, vmReq, logEntry)
		}
	}
}

func (s *cubeHostImageReverseClient) asyncDispatch(ctx context.Context,
	client cubehost.CubeHostImageService_ReverseStreamForwardImageClient,
	vmReq *cubehost.ServerMessage,
	logEntry *log.CubeWrapperLogEntry) {
	if vmReq.Type == cubehost.MessageType_CLIENT_HELLO {
		return
	}

	go func() {
		s.lock.Lock()
		s.inflatingRequestMap[vmReq.Id] = vmReq
		s.lock.Unlock()

		defer func() {
			s.lock.Lock()
			delete(s.inflatingRequestMap, vmReq.Id)
			s.lock.Unlock()
		}()
		switch vmReq.Type {
		case cubehost.MessageType_LIST_INFLIGHT_REQUEST:
			s.handleListInflatingRequest(ctx, client, vmReq)
		case cubehost.MessageType_IMAGE_FORWARD:
			s.handleForwardImage(ctx, client, vmReq)
		case cubehost.MessageType_REMOVE_SNAPSHOT:
			s.handleRemoveSnapshot(ctx, client, vmReq)
		case cubehost.MessageType_IMAGE_FS_STATS:
			s.handleFsStats(ctx, client, vmReq)
		default:
			logEntry.Errorf("unknown message type: %v", vmReq.Type)
		}
	}()
}

func (s *cubeHostImageReverseClient) updateHeartBeat() {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.heartBeatTime = time.Now()
}

func (s *cubeHostImageReverseClient) handleListInflatingRequest(
	ctx context.Context,
	client cubehost.CubeHostImageService_ReverseStreamForwardImageClient,
	vmReq *cubehost.ServerMessage) {
	var inflatingRequestList = &cubehost.ListInflightRequest{}

	s.updateHeartBeat()
	s.lock.Lock()
	for id := range s.inflatingRequestMap {
		inflatingRequestList.Requests = append(inflatingRequestList.Requests, &cubehost.InflightRequest{
			Id: id,
		})
	}
	s.lock.Unlock()

	err := client.Send(&cubehost.ClientMessage{
		Id:   vmReq.Id,
		Type: cubehost.MessageType_LIST_INFLIGHT_REQUEST,
		Status: &cubehost.ResponseStatus{
			Code:    cubehost.ResponseStatusCode_OK,
			Message: "",
		},
		Content: &cubehost.ClientMessage_ListInflightRequest{
			ListInflightRequest: inflatingRequestList,
		},
	})
	if err != nil {
		s.sendError(fmt.Errorf("failed to send list inflating request response: %w", err))
		return
	}
}

func (s *cubeHostImageReverseClient) handleFsStats(ctx context.Context, client cubehost.CubeHostImageService_ReverseStreamForwardImageClient, vmReq *cubehost.ServerMessage) bool {
	var (
		code  = cubehost.ResponseStatusCode_OK
		msg   = ""
		start = time.Now()
	)
	logEntry := log.G(ctx).WithFields(CubeLog.Fields{
		"requestID": vmReq.Id,
		"step":      "handle fs stat",
	})

	var (
		fsStat  = &cubehost.ImageFsStats{}
		sandbox = s.updater.GetSandboxer()
	)

	if sandbox == nil {
		code = cubehost.ResponseStatusCode_ERROR
		msg = "sandbox is nil"
	} else {
		podConfig := sandbox.GetOrCreatePodConfig()
		fsStat.CapacityBytes = uint64(podConfig.ImageStorageQuota)
		fsStat.AvailableBytes = uint64(podConfig.ImageStorageQuota - podConfig.ImageStorageUsed)
		fsStat.UsedBytes = uint64(podConfig.ImageStorageUsed)
	}
	err := client.Send(&cubehost.ClientMessage{
		Id:   vmReq.Id,
		Type: cubehost.MessageType_IMAGE_FS_STATS,
		Status: &cubehost.ResponseStatus{
			Code:    code,
			Message: msg,
		},
		Content: &cubehost.ClientMessage_ImageFsStats{
			ImageFsStats: fsStat,
		},
	})
	logEntry.WithFields(CubeLog.Fields{
		"duration": time.Since(start).String(),
		"fsStat":   log.WithJsonValue(fsStat),
	}).Debug(msg)
	if err != nil {
		s.sendError(fmt.Errorf("failed to send fs stat response: %w", err))
		return true
	}
	return false
}

func (s *cubeHostImageReverseClient) handleRemoveSnapshot(ctx context.Context, client cubehost.CubeHostImageService_ReverseStreamForwardImageClient, vmReq *cubehost.ServerMessage) bool {
	var (
		code  = cubehost.ResponseStatusCode_OK
		msg   = ""
		start = time.Now()
	)
	toRemove := vmReq.GetRemoveSnapshotRequest().GetLayerMounts()
	v, _ := json.Marshal(toRemove)
	log := log.G(ctx).WithFields(CubeLog.Fields{
		"toRemove":  v,
		"requestID": vmReq.Id,
		"step":      "handle remove snapshot",
	})

	err := s.updater.RemoveLayerMounts(ctx, toRemove)
	if err != nil {

		code = cubehost.ResponseStatusCode_ERROR
		msg = fmt.Sprintf("ignored failed to remove layer mount: %v", err)
	}
	log.WithField("duration", time.Since(start).String()).Info(msg)
	err = client.Send(&cubehost.ClientMessage{
		Id:   vmReq.Id,
		Type: cubehost.MessageType_REMOVE_SNAPSHOT,
		Status: &cubehost.ResponseStatus{
			Code:    code,
			Message: msg,
		},
		Content: &cubehost.ClientMessage_Common{
			Common: "",
		},
	})
	if err != nil {
		s.sendError(fmt.Errorf("failed to send remove snapshot response: %w", err))
		return true
	}
	return false
}

func (s *cubeHostImageReverseClient) initReverseStreamForwardImageClient(ctx context.Context, vmClient cubehost.CubeHostImageServiceClient) (cubehost.CubeHostImageService_ReverseStreamForwardImageClient, error) {
	reverseClient, err := vmClient.ReverseStreamForwardImage(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create image forward client: %v", err)
	}

	msg := &cubehost.ClientMessage{
		Id:   uuid.NewString(),
		Type: cubehost.MessageType_CLIENT_HELLO,
		Content: &cubehost.ClientMessage_Hello{
			Hello: "hello",
		},
	}

	err = reverseClient.Send(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to send client hello message: %w", err)
	}
	_, err = reverseClient.Recv()
	if err != nil {
		return nil, fmt.Errorf("failed to receive server hello message: %w", err)
	}
	err = reverseClient.Send(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to send client hello message 2: %w", err)
	}
	return reverseClient, nil
}

func (s *cubeHostImageReverseClient) cubeHostDialer(ctx context.Context, address string) (net.Conn, error) {
	ctx = namespaces.WithNamespace(ctx, s.ns)
	if s.updater.GetSandboxer() == nil || !s.isRunningContainer() {
		err := fmt.Errorf("sandbox %s running", s.factory.sandboxID)
		if s.grpcClient != nil {
			s.grpcClient.Close()
		}
		return nil, err
	}
	return s.factory.CreateCubeHostConn(ctx)
}

const (
	mockCubeHostImageAddress = "localhost:1027"
)

func (s *cubeHostImageReverseClient) createCubeHostClient() (*grpc.ClientConn, error) {

	backoffConfig := DefaultConfig
	connParams := grpc.ConnectParams{
		Backoff: backoffConfig,
	}
	gopts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(connParams),
		grpc.WithContextDialer(s.cubeHostDialer),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(defaults.DefaultMaxRecvMsgSize)),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(defaults.DefaultMaxSendMsgSize)),
	}
	client, err := grpc.NewClient(mockCubeHostImageAddress, gopts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create cubehost grpc client: %v", err)
	}

	return client, nil
}

func (s *cubeHostImageReverseClient) Close() error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if !s.isClosed {
		s.cancel()
		close(s.closeChan)
		s.isClosed = true
	}
	if s.grpcClient != nil {
		s.grpcClient.Close()
		s.grpcClient = nil
	}
	if !s.errChanClosed {
		close(s.errChan)
		s.errChanClosed = true
	}

	return nil
}

func (s *cubeHostImageReverseClient) sendError(err error) {
	if !s.errChanClosed {
		s.errChan <- err
		s.errChanClosed = true
	}
}

func (s *cubeHostImageReverseClient) GetVmClient() cubehost.CubeHostImageServiceClient {
	return s.vmClient
}

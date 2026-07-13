// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package images

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/containerd/plugin/registry"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/platforms"
	"github.com/containerd/plugin"
	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/server/images/ext4image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/chi"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/cubelog"
	"google.golang.org/grpc"
)

var _ images.ImagesServer = (*service)(nil)

type ImageServiceConfig struct {
	ImageGCConfig ImageGCConfig `toml:"image_gc"`
}

type ImageGCConfig struct {
	FreeDiskThresholdPercent int    `toml:"free_disk_threshold_percent"`
	MaxUnusedTimeIntervalStr string `toml:"max_unused_time_interval"`
	MinAgeStr                string `toml:"min_age"`
	DetectionIntervalStr     string `toml:"detection_interval"`
	MaxDeletionPerCycle      int    `toml:"max_deletion_per_cycle"`
	DataPath                 string `toml:"data_path"`
}

func init() {
	registry.Register(&plugin.Registration{
		Type:   constants.ImagesServicePlugin,
		ID:     constants.ImagesServiceID.ID(),
		Config: &ImageServiceConfig{},
		Requires: []plugin.Type{
			constants.InternalPlugin,
			constants.CubeboxServicePlugin,
			constants.PluginChi,
			constants.CubeStorePlugin,
		},
		InitFn: initFunc,
	})
}

func initFunc(ic *plugin.InitContext) (interface{}, error) {
	internals, err := ic.GetByType(constants.InternalPlugin)
	if err != nil {
		return nil, err
	}
	i, ok := internals[constants.ImagesID.ID()]
	if !ok {
		return nil, fmt.Errorf("%v plugin not found", constants.ImagesID)
	}

	v, ok := internals[constants.VolumeSourceID.ID()]
	if !ok {
		return nil, fmt.Errorf("%v plugin not found", constants.ImagesID)
	}

	client, err := containerd.New(
		"",
		containerd.WithDefaultPlatform(platforms.Default()),
		containerd.WithInMemoryServices(ic),
	)
	if err != nil {
		return nil, fmt.Errorf("init containerd connect failed.%s", err)
	}

	srv := &service{
		l:      i.(*local),
		volume: v.(*volumeLocal),
		client: client,
	}

	cubeletOpts, err := cubelet.WithInMemoryService(ic)
	if err != nil {
		return nil, fmt.Errorf("init cubelet client failed.%s", err)
	}
	cubeletOpts = append(cubeletOpts, cubelet.WithImageClient(srv))
	cubeletClient, err := cubelet.New("", cubelet.WithServices(cubeletOpts...))
	if err != nil {
		return nil, fmt.Errorf("init containerd connect failed.%s", err)
	}

	config := ic.Config.(*ImageServiceConfig)
	policy := parseImageGCPolicy(config.ImageGCConfig)

	cf, err := ic.GetByID(constants.PluginChi, constants.PluginVSocketManger)
	if err != nil {
		return nil, err
	}
	chif, ok := cf.(chi.ChiFactory)
	if !ok {
		return nil, fmt.Errorf("invalid chi factory type")
	}
	srv.imageGCManager = NewImageGCManager(cubeletClient, srv, srv.l.criImage, policy, chif)

	cubeboxAPIObj, err := ic.GetByID(constants.CubeStorePlugin, constants.CubeboxID.ID())
	if err != nil {
		return nil, fmt.Errorf("get cubebox api client fail:%v", err)
	}
	if register, ok := cubeboxAPIObj.(cubes.CubeboxEventListenerRegistry); ok {
		register.Register(srv.imageGCManager)
	}
	if usage, ok := cubeboxAPIObj.(imageUsageChecker); ok {
		srv.imageUsage = usage
	}
	go srv.imageGCManager.Run(ic.Context)

	return srv, nil
}

func parseImageGCPolicy(c ImageGCConfig) ImageGCPolicy {
	policy := ImageGCPolicy{
		MaxDeletionPerCycle: c.MaxDeletionPerCycle,
	}

	if c.FreeDiskThresholdPercent > 0 && c.FreeDiskThresholdPercent <= 100 {
		policy.FreeDiskThresholdPercent = c.FreeDiskThresholdPercent
	}

	t, err := time.ParseDuration(c.MaxUnusedTimeIntervalStr)
	if err != nil || t == 0 {
		policy.LeastUnusedTimeInterval = 24 * time.Hour
	} else {
		policy.LeastUnusedTimeInterval = t
	}

	t, err = time.ParseDuration(c.MinAgeStr)
	if err != nil || t == 0 {
		policy.MinAge = 10 * time.Minute
	} else {
		policy.MinAge = t
	}

	t, err = time.ParseDuration(c.DetectionIntervalStr)
	if err != nil || t == 0 {
		policy.DetectionInterval = 1 * time.Minute
	} else {
		policy.DetectionInterval = t
	}

	if c.DataPath == "" {
		c.DataPath = "/data/cubelet/root"
	}
	policy.DataPath = c.DataPath
	return policy
}

// imageUsageChecker reports whether an image/artifact id is still referenced by
// a running sandbox on this node. Implemented by the cubebox store plugin; used
// by the ext4 artifact destroy path to refuse deleting an in-use OS image.
type imageUsageChecker interface {
	IsImageInUse(imageID string) (bool, error)
}

type service struct {
	l              *local
	imageGCManager *imageGCManager
	client         *containerd.Client
	volume         *volumeLocal
	imageUsage     imageUsageChecker
	images.UnimplementedImagesServer
}

func (s *service) RegisterTCP(server *grpc.Server) error {
	images.RegisterImagesServer(server, s)
	return nil
}

func (s *service) Register(server *grpc.Server) error {
	images.RegisterImagesServer(server, s)
	return nil
}

func (s *service) CreateImage(ctx context.Context, req *images.CreateImageRequest) (*images.CreateImageRequestResponse, error) {
	rsp := &images.CreateImageRequestResponse{
		RequestID: req.RequestID,
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	rt := &CubeLog.RequestTrace{
		Action:       "CreateImage",
		RequestID:    req.RequestID,
		Caller:       constants.ImagesServiceID.ID(),
		Callee:       constants.ImagesServiceID.ID(),
		CalleeAction: "CreateImage",
	}

	ctx = CubeLog.WithRequestTrace(ctx, rt)
	log.G(ctx).Errorf("CreateImageRequest:%s", utils.InterfaceToString(req))

	start := time.Now()
	defer func() {
		if !ret.IsSuccessCode(rsp.Ret.RetCode) {
			log.G(ctx).WithFields(map[string]interface{}{
				"RetCode": int64(rsp.Ret.RetCode),
			}).Errorf("CreateImage fail:%+v", rsp)
		}
		rt.Cost = time.Since(start)
		rt.RetCode = int64(rsp.Ret.RetCode)
		CubeLog.Trace(rt)
	}()
	defer recov.HandleCrash(func(panicError interface{}) {
		log.G(ctx).Fatalf("CreateImage panic info:%s, stack:%s", panicError, string(debug.Stack()))
		rsp.Ret.RetMsg = string(debug.Stack())
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
	})

	if req.GetSpec() == nil {
		rsp.Ret.RetMsg = "Spec should be provided"
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}

	ns := namespaces.Default
	if req.Spec.Namespace != "" {
		ns = req.Spec.Namespace
	}
	ctx = namespaces.WithNamespace(ctx, ns)
	ctx = constants.WithImageSpec(ctx, req.GetSpec())
	image, err := s.l.criImage.EnsureImage(ctx, req.GetSpec().GetImage(),
		req.GetSpec().GetUsername(),
		req.GetSpec().GetToken(),
		&runtime.PodSandboxConfig{})
	if err != nil {
		er, _ := ret.FromError(err)
		rsp.Ret.RetMsg = fmt.Sprintf("pull image %v: %v", req.GetSpec().GetImage(), er.Message())
		rsp.Ret.RetCode = er.Code()
		return rsp, nil
	}
	if image != nil {
		log.G(ctx).Debugf("image pulled succ:%v", image.Name())
	}
	return rsp, nil
}

func (s *service) DestroyImage(ctx context.Context, req *images.DestroyImageRequest) (*images.DestroyImageResponse, error) {
	rsp := &images.DestroyImageResponse{
		RequestID: req.RequestID,
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	rt := &CubeLog.RequestTrace{
		Action:       "DestroyImage",
		RequestID:    req.RequestID,
		Caller:       constants.ImagesServiceID.ID(),
		Callee:       constants.ImagesServiceID.ID(),
		CalleeAction: "DestroyImage",
	}

	ctx = CubeLog.WithRequestTrace(ctx, rt)
	log.G(ctx).Errorf("DestroyImage:%s", cubeimages.SafePrintImageSpec(req.GetSpec()))

	start := time.Now()
	defer func() {
		if rsp.Ret.RetCode != errorcode.ErrorCode_Success && rsp.Ret.RetCode != errorcode.ErrorCode_OK {
			log.G(ctx).WithFields(map[string]interface{}{
				"RetCode": int64(rsp.Ret.RetCode),
			}).Errorf("DestroyImage fail:%+v", rsp)
		}
		rt.Cost = time.Since(start)
		rt.RetCode = int64(rsp.Ret.RetCode)
		CubeLog.Trace(rt)
	}()
	defer recov.HandleCrash(func(panicError interface{}) {
		log.G(ctx).Fatalf("DestroyImage panic info:%s, stack:%s", panicError, string(debug.Stack()))
		rsp.Ret.RetMsg = string(debug.Stack())
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
	})

	if req.GetSpec() == nil {
		rsp.Ret.RetMsg = "Spec should be provided"
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		return rsp, nil
	}

	// FIX-1: ext4 OS image artifacts are not containerd images; the legacy
	// CRI RemoveImage path silently no-ops on them (LocalResolve NotFound).
	// Route ext4 deletions to the synchronous, idempotent pmem destroy path.
	if req.GetSpec().GetStorageMedia() == cubeimages.ImageStorageMediaType_ext4.String() {
		return s.destroyExt4Artifact(ctx, req, rsp), nil
	}

	ns := namespaces.Default
	if req.Spec.Namespace != "" {
		ns = req.Spec.Namespace
	}
	if clins, ok := req.Spec.Annotations[constants.AnnotationCubeletNameSpace]; ok {
		ns = clins
	}
	ctx = namespaces.WithNamespace(ctx, ns)
	err := s.l.criImage.RemoveImage(ctx, &runtime.ImageSpec{
		Image: req.GetSpec().GetImage(),
	})
	if err != nil {
		er, _ := ret.FromError(err)
		rsp.Ret.RetMsg = fmt.Sprintf("Remove image %v: %v", req.GetSpec().GetImage(), er.Message())
		rsp.Ret.RetCode = er.Code()
		return rsp, nil
	}
	log.G(ctx).Debugf("Remove image succ:%v", utils.InterfaceToString(req.GetSpec()))
	return rsp, nil
}

// destroyExt4Artifact removes an ext4 OS image artifact's on-disk files. The
// instanceType is taken from the MasterAnnotationInstanceType annotation (set
// symmetrically by CreateImage distribution), defaulting to the cubebox
// instance type (currently the only ext4 instance type). A running-sandbox
// reference results in a Conflict so an in-use OS image is never deleted;
// CubeMaster GC retries after the sandbox exits.
func (s *service) destroyExt4Artifact(ctx context.Context, req *images.DestroyImageRequest, rsp *images.DestroyImageResponse) *images.DestroyImageResponse {
	artifactID := req.GetSpec().GetImage()
	instanceType := strings.TrimSpace(req.GetSpec().GetAnnotations()[constants.MasterAnnotationInstanceType])
	if instanceType == "" {
		instanceType = cubebox.InstanceType_cubebox.String()
	}

	var inUse ext4image.ArtifactInUseFunc
	if s.imageUsage != nil {
		inUse = s.imageUsage.IsImageInUse
	}

	err := ext4image.DestroyPmemArtifact(ctx, instanceType, artifactID, inUse)
	if err == nil {
		log.G(ctx).Infof("destroyed ext4 artifact %q (instanceType=%s)", artifactID, instanceType)
		return rsp
	}
	if errors.Is(err, ext4image.ErrArtifactInUse) {
		rsp.Ret.RetCode = errorcode.ErrorCode_Conflict
		rsp.Ret.RetMsg = err.Error()
		return rsp
	}
	rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
	rsp.Ret.RetMsg = fmt.Sprintf("destroy ext4 artifact %q: %v", artifactID, err)
	return rsp
}

type ExpirationTimeSetter interface {
	SetImageExpirationTime(t time.Duration) error
	SetCodeExpirationTime(t time.Duration) error
}

func (s *service) SetImageExpirationTime(t time.Duration) error {
	if t.Hours() < 24 {
		return fmt.Errorf("too short expiration time, must be greater or equal than 24 hours")
	}
	if t != s.imageGCManager.policy.LeastUnusedTimeInterval {
		s.imageGCManager.policy.LeastUnusedTimeInterval = t
		CubeLog.Errorf("set image expiration time to %v", t)
	}

	return nil
}

func (s *service) SetCodeExpirationTime(t time.Duration) error {
	return s.volume.SetExpirationTime(t)
}

func (s *service) ListNamespaces(ctx context.Context) ([]string, error) {
	return s.client.NamespaceService().List(ctx)
}

var _ images.ImagesServer = &service{}

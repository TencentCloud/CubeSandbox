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

package images

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	imagedigest "github.com/opencontainers/go-digest"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.uber.org/mock/gomock"
	"google.golang.org/grpc"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	criconfig "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/config"
	imagestore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/image"
	snapshotstore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/snapshot"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/kmutex"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/multimeta"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

const (
	testImageFSPath = "/test/image/fs/path"

	testSandboxImage = "sha256:c75bebcdd211f41b3a460c7bf82970ed6c75acaab9cd4c9a4e125b03ca113798"
)

type imageClient interface {
	ListImages(context.Context, ...string) ([]containerd.Image, error)
	GetImage(context.Context, string) (containerd.Image, error)
	Pull(context.Context, string, ...containerd.RemoteOpt) (containerd.Image, error)

	SnapshotService(snapshotterName string) snapshots.Snapshotter
}

type ImagePlatform struct {
	Snapshotter string
	Platform    imagespec.Platform
}

type CubeImageSvcInterface interface {
	EnsureImage(ctx context.Context, ref string, username string, password string, config *runtime.PodSandboxConfig) (containerd.Image, error)
	GetImage(ctx context.Context, id string) (imagestore.Image, error)

	ListImage(ctx context.Context) ([]imagestore.Image, error)
	LocalResolve(ctx context.Context, refOrID string) (imagestore.Image, error)
	PullImage(ctx context.Context, name string, credentials func(string) (string, string, error), sandboxConfig *runtime.PodSandboxConfig) (_ string, err error)
	RemoveImage(ctx context.Context, imageSpec *runtime.ImageSpec) error
}

type CubeImageDeletingScheduler interface {
	RemoveImage(ctx context.Context, imageSpec *runtime.ImageSpec) error
	Reschedule(ctx context.Context) error
}

type CubeImageService struct {
	config criconfig.ImageConfig

	images images.Store

	client *containerd.Client

	imageFSPaths map[string]string

	runtimePlatforms map[string]ImagePlatform

	imageStore *imagestore.Store

	snapshotStore *snapshotstore.Store

	unpackDuplicationSuppressor kmutex.KeyedLocker

	uidDir string

	defaultSnapshotter string

	content content.Store

	metadata *metadata.DB
	ss       *snapshotsSyncer

	pullImageLockMap *sync.Map

	imageRemoveLock *sync.RWMutex

	imageDeleteScheduler *imageDeleteScheduler
}

type CRIImageServiceOptions struct {
	Metadata *metadata.DB
	Content  content.Store

	Images images.Store

	ImageFSPaths map[string]string

	RuntimePlatforms map[string]ImagePlatform

	Snapshotters       map[string]snapshots.Snapshotter
	DefaultSnapshotter string

	Client *containerd.Client

	RootDir string

	MetaDB multimeta.MetadataDBAPI
}

func NewTestCRIService() (*CubeImageService, *GRPCCRIImageService) {
	service := &CubeImageService{
		config:           testImageConfig,
		runtimePlatforms: map[string]ImagePlatform{},
		imageFSPaths:     map[string]string{"overlayfs": testImageFSPath},
		imageStore:       imagestore.NewStore(nil, nil, platforms.Default()),
		snapshotStore:    snapshotstore.NewStore(),
		pullImageLockMap: &sync.Map{},
		imageRemoveLock:  &sync.RWMutex{},
	}

	return service, &GRPCCRIImageService{service}
}

func NewTestCRIServiceWithImageStore(imageStore *imagestore.Store, ctrl *gomock.Controller) (CubeImageSvcInterface, *GRPCCRIImageService) {
	service := &CubeImageService{
		config:           testImageConfig,
		runtimePlatforms: map[string]ImagePlatform{},
		imageFSPaths:     map[string]string{"overlayfs": testImageFSPath},
		imageStore:       imageStore,
		snapshotStore:    snapshotstore.NewStore(),
		pullImageLockMap: &sync.Map{},
		imageRemoveLock:  &sync.RWMutex{},
	}

	return service, &GRPCCRIImageService{service}
}

var testImageConfig = criconfig.ImageConfig{
	PinnedImages: map[string]string{
		"sandbox": testSandboxImage,
	},
}

func NewService(config criconfig.ImageConfig, options *CRIImageServiceOptions) (CubeImageSvcInterface, error) {
	svc := CubeImageService{
		config:                      config,
		images:                      options.Images,
		client:                      options.Client,
		imageStore:                  imagestore.NewStore(options.Images, options.Content, platforms.Default()),
		imageFSPaths:                options.ImageFSPaths,
		runtimePlatforms:            options.RuntimePlatforms,
		snapshotStore:               snapshotstore.NewStore(),
		unpackDuplicationSuppressor: kmutex.New(),
		uidDir:                      filepath.Join(options.RootDir, "uids"),
		defaultSnapshotter:          options.DefaultSnapshotter,
		content:                     options.Content,
		metadata:                    options.Metadata,
		pullImageLockMap:            &sync.Map{},
		imageRemoveLock:             &sync.RWMutex{},
	}

	svc.imageDeleteScheduler = newImageDeleteScheduler(&svc, options.MetaDB)

	CubeLog.Infof("Start snapshots syncer with uid directory: %s", svc.uidDir)
	snapshotsSyncer := newSnapshotsSyncer(
		svc.snapshotStore,
		options.Snapshotters,
		time.Duration(svc.config.StatsCollectPeriod)*time.Second,
	)
	snapshotsSyncer.start()
	svc.ss = snapshotsSyncer
	err := os.MkdirAll(svc.uidDir, 0755)
	if err != nil {
		return nil, fmt.Errorf("failed to create uid directory %s: %w", svc.uidDir, err)
	}
	initMetrics(context.Background())
	return &svc, nil
}

func (c *CubeImageService) LocalResolve(ctx context.Context, refOrID string) (imagestore.Image, error) {
	getImageID := func(refOrId string) string {
		if _, err := imagedigest.Parse(refOrID); err == nil {
			return refOrID
		}
		return func(ref string) string {

			normalized, err := reference.ParseDockerRef(ref)
			if err != nil {
				return ""
			}
			id, err := c.imageStore.Resolve(ctx, normalized.String())
			if err != nil {
				return ""
			}
			return id
		}(refOrID)
	}

	imageID := getImageID(refOrID)
	if imageID == "" {

		imageID = refOrID
	}
	return c.imageStore.Get(ctx, imageID)
}

func (c *CubeImageService) ToContainerdImage(ctx context.Context, image imagestore.Image) (containerd.Image, error) {

	if len(image.References) == 0 {
		return nil, fmt.Errorf("invalid image with no reference %q", image.ID)
	}

	return c.client.GetImage(ctx, image.ID)
}

func (c *CubeImageService) RuntimeSnapshotter(ctx context.Context, ociRuntime criconfig.Runtime) string {
	if ociRuntime.Snapshotter == "" {
		return c.config.Snapshotter
	}

	log.G(ctx).Debugf("Set snapshotter for runtime %s to %s", ociRuntime.Type, ociRuntime.Snapshotter)
	return ociRuntime.Snapshotter
}

func (c *CubeImageService) ListImage(ctx context.Context) ([]imagestore.Image, error) {
	var nses []string
	ns, err := namespaces.NamespaceRequired(ctx)
	if err == nil {
		nses = append(nses, ns)
	} else {
		ns, err := c.client.NamespaceService().List(ctx)
		if err != nil {
			log.G(ctx).Errorf("failed to list namespaces while list all images: %v", err)
			nses = append(nses, namespaces.Default)
		} else {
			nses = append(nses, ns...)
		}
	}

	var allImages []imagestore.Image
	for _, ns := range nses {
		ctx = namespaces.WithNamespace(ctx, ns)
		imagesInStore, err := c.imageStore.List(ctx)
		if err != nil {
			return nil, err
		}

		allImages = append(allImages, imagesInStore...)
	}

	return allImages, nil
}

func (c *CubeImageService) GetImage(ctx context.Context, id string) (imagestore.Image, error) {
	return c.imageStore.Get(ctx, id)
}

func (c *CubeImageService) GetSnapshot(key, snapshotter string) (snapshotstore.Snapshot, error) {
	snapshotKey := snapshotstore.Key{
		Key:         key,
		Snapshotter: snapshotter,
	}
	return c.snapshotStore.Get(snapshotKey)
}

func (c *CubeImageService) ImageFSPaths() map[string]string {
	return c.imageFSPaths
}

func (c *CubeImageService) PinnedImage(name string) string {
	return c.config.PinnedImages[name]
}

type GRPCCRIImageService struct {
	*CubeImageService
}

func (c *CubeImageService) RegisterTCP(server *grpc.Server) error {
	runtime.RegisterImageServiceServer(server, &GRPCCRIImageService{c})
	return nil
}

func (c *CubeImageService) Register(server *grpc.Server) error {
	runtime.RegisterImageServiceServer(server, &GRPCCRIImageService{c})
	return nil
}

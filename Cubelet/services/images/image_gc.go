// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package images

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/hashicorp/go-multierror"
	imagedigest "github.com/opencontainers/go-digest"
	"k8s.io/apimachinery/pkg/util/sets"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	criimages "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/server/images"
	imagestore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cdp"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/chi"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

type imageRecord struct {
	ns string
	*imagestore.Image

	lastUsed time.Time

	detectedTime time.Time
}

func (r *imageRecord) ID() string {
	return r.ns + "/" + r.Image.ID
}

type evictionInfo struct {
	*imageRecord
}

func evictionByCreatedTime(e1, e2 *evictionInfo) bool {
	return e1.ImageSpec.Created.Before(*e2.ImageSpec.Created)
}

func evictionByLastUsedTime(e1, e2 *evictionInfo) bool {
	return e1.lastUsed.Before(e2.lastUsed)
}

func evictionBySize(e1, e2 *evictionInfo) bool {
	return e1.Size < e2.Size
}

type ImageGCPolicy struct {
	DataPath string

	FreeDiskThresholdPercent int

	LeastUnusedTimeInterval time.Duration

	MinAge time.Duration

	DetectionInterval time.Duration

	MaxDeletionPerCycle int
}

type imageGCManager struct {
	imageRecords     map[string]*imageRecord
	imageRecordsLock sync.Mutex

	policy ImageGCPolicy

	cubeletClient *cubelet.Client
	lister        NamespaceLister

	criImage criimages.CubeImageSvcInterface

	triggerCh chan struct{}

	getDeviceIdleRatioFunc func(path string) (uint64, uint64, error)

	chif chi.ChiFactory
}

type NamespaceLister interface {
	ListNamespaces(ctx context.Context) ([]string, error)
}

func NewImageGCManager(cubeletClient *cubelet.Client, lister NamespaceLister, criImage criimages.CubeImageSvcInterface, policy ImageGCPolicy, chif chi.ChiFactory) *imageGCManager {
	CubeLog.Infof("image gc policy: %+v", policy)
	m := &imageGCManager{
		imageRecords:           make(map[string]*imageRecord),
		policy:                 policy,
		cubeletClient:          cubeletClient,
		lister:                 lister,
		criImage:               criImage,
		triggerCh:              make(chan struct{}, 1),
		getDeviceIdleRatioFunc: utils.GetDeviceIdleRatio,
		chif:                   chif,
	}

	return m
}

func (m *imageGCManager) HouseKeepingScheduler() {
	t := time.NewTicker(m.policy.DetectionInterval)
	for range t.C {
		select {
		case m.triggerCh <- struct{}{}:
		default:

		}
	}
}

func (m *imageGCManager) Run(ctx context.Context) {
	go m.HouseKeepingScheduler()

	rt := &CubeLog.RequestTrace{
		Action: "ImageGC",
		Caller: constants.ImagesID.ID(),
	}
	ctx = CubeLog.WithRequestTrace(ctx, rt)
	ctx = log.WithLogger(ctx, log.NewWrapperLogEntry(log.AuditLogger.WithContext(ctx)))
	for {
		select {
		case <-m.triggerCh:
		case <-ctx.Done():
			return
		}

		workCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)

		if ds, ok := m.criImage.(criimages.CubeImageDeletingScheduler); ok {
			ds.Reschedule(ctx)
		}

		err := m.GarbageCollect(workCtx)
		cancel()
		if err != nil {
			log.G(workCtx).Errorf("image gc: %v", err)
		} else {
			log.G(workCtx).Debugf("Successfully image gc")
		}
	}
}

func (m *imageGCManager) GarbageCollect(ctx context.Context) error {
	defer utils.Recover()

	imagesInUse, err := m.updateImageInUse(ctx)
	if err != nil {
		return err
	}

	freeblockPercentage, _, err := m.getDeviceIdleRatioFunc(m.policy.DataPath)
	if err != nil {
		return fmt.Errorf("failed to get disk %q idle ratio: %v", m.policy.DataPath, err)
	}

	var (
		imagesToDel  = make([]evictionInfo, 0)
		diskPressure = int(freeblockPercentage) < m.policy.FreeDiskThresholdPercent
		now          = time.Now()
	)

	for key, record := range m.imageRecords {

		if _, ok := imagesInUse[key]; ok {
			record.lastUsed = now
			continue
		}

		if !diskPressure &&
			(record.Image.MediaType == cubeimages.ImageStorageMediaType_docker.String() ||
				record.Image.MediaType == "") {
			continue
		}

		if record.Pinned {
			continue
		}

		if now.Sub(record.detectedTime) < m.policy.MinAge {
			continue
		}

		if now.Sub(record.lastUsed) < m.policy.LeastUnusedTimeInterval {
			continue
		}

		imagesToDel = append(imagesToDel, evictionInfo{
			imageRecord: record,
		})
	}

	utils.OrderedBy(evictionByLastUsedTime, evictionByCreatedTime, evictionBySize).Sort(imagesToDel)

	deleted := 0
	var result *multierror.Error
	for _, image := range imagesToDel {
		tmpCtx := namespaces.WithNamespace(ctx, image.ns)
		if err := m.criImage.RemoveImage(tmpCtx, &runtime.ImageSpec{Image: image.Image.ID}); err != nil {
			result = multierror.Append(result, fmt.Errorf("destroy image %q: %w", image.ID(), err))
			continue
		}
		log.G(tmpCtx).Errorf("image %v(%v) is garbage collected, lastUsed in %v ago",
			image.References, image.ID(), time.Since(image.lastUsed))

		deleted++
		if deleted >= m.policy.MaxDeletionPerCycle {
			break
		}
	}

	if deleted > 0 {
		trace := &CubeLog.RequestTrace{
			Action:  "ImageGC",
			Callee:  constants.ImagesID.ID(),
			RetCode: int64(deleted),
		}
		CubeLog.Trace(trace)
	}
	return result.ErrorOrNil()
}

type inuseImage struct {
	ns string
	*imagestore.Image
}

func newInuseImage(ns string, image *imagestore.Image) *inuseImage {
	return &inuseImage{
		ns:    ns,
		Image: image,
	}
}

func (ii *inuseImage) ID() string {
	return ii.ns + "/" + ii.Image.ID
}

func (m *imageGCManager) OnCubeboxEvent(ctx context.Context, event *cubes.CubeboxEvent) error {
	if event == nil || event.Cubebox == nil {
		return nil
	}

	var imageRefs []string

	for _, ctr := range event.Cubebox.AllContainers() {
		if _, ok := ctr.Labels[constants.LabelContainerImagePem]; ok {
			continue
		}
		imageRefs = append(imageRefs, ctr.GetContainerImageIDs()...)
	}
	if len(imageRefs) > 0 && m.criImage != nil {
		m.imageRecordsLock.Lock()
		defer m.imageRecordsLock.Unlock()

		now := time.Now()
		for _, ref := range imageRefs {
			img, err := m.criImage.LocalResolve(ctx, ref)
			if err != nil {
				continue
			}

			key := event.Cubebox.Namespace + "/" + img.ID
			if _, ok := m.imageRecords[key]; ok {
				m.imageRecords[key].lastUsed = now
				continue
			} else {
				m.imageRecords[key] = &imageRecord{
					ns:           event.Cubebox.Namespace,
					Image:        &img,
					detectedTime: now,
					lastUsed:     now,
				}
			}
		}
	}
	return nil
}

func (m *imageGCManager) updateImageInUse(ctx context.Context) (map[string]*inuseImage, error) {

	resp, err := m.cubeletClient.CubeBoxService().List(ctx, &cubebox.ListCubeSandboxRequest{})
	if err != nil {
		return nil, fmt.Errorf("list cubebox: %w", err)
	}

	var (
		imagesInUse = make(map[string]*inuseImage)
		now         = time.Now()
	)

	for _, box := range resp.GetItems() {
		nsCtx := namespaces.WithNamespace(ctx, box.Namespace)
		for _, ctr := range box.GetContainers() {
			if _, ok := ctr.Labels[constants.LabelContainerImagePem]; ok {
				continue
			}

			named := checkImageCosTypeID(ctr.Labels)
			if named == "" {
				image := ctr.GetImage()
				if _, err := imagedigest.Parse(image); err == nil {
					named = image
				} else {
					namedS, err := reference.ParseDockerRef(image)
					if err != nil {
						continue
					}
					named = namedS.String()
				}
			}

			image, err := m.criImage.LocalResolve(nsCtx, named)
			if errdefs.IsNotFound(err) {
				continue
			} else if err != nil {
				return nil, fmt.Errorf("failed to resolve image %q: %w", named, err)
			}

			iim := newInuseImage(box.Namespace, &image)
			imagesInUse[iim.ID()] = iim
		}
	}

	if m.chif != nil {
		if cmImages, err := m.chif.ListAllVmImages(ctx); err != nil {
			log.G(ctx).Errorf("failed to gc image, cause by list all vm images: %v", err)
			return nil, err
		} else {
			for ns, imageRefs := range cmImages {
				nsCtx := namespaces.WithNamespace(ctx, ns)
				for _, imageRef := range imageRefs.UnsortedList() {
					image, err := m.criImage.LocalResolve(nsCtx, imageRef)
					if errdefs.IsNotFound(err) {
						continue
					}
					iim := newInuseImage(ns, &image)
					imagesInUse[iim.ID()] = iim
				}
			}
		}
	}

	m.imageRecordsLock.Lock()
	defer m.imageRecordsLock.Unlock()

	nses, err := m.lister.ListNamespaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}

	currentImages := sets.NewString()
	for _, ns := range nses {
		nsCtx := namespaces.WithNamespace(ctx, ns)
		nsImages, err := m.criImage.ListImage(nsCtx)
		if err != nil && !errdefs.IsNotFound(err) {
			return nil, fmt.Errorf("list images in namespace %q: %w", ns, err)
		}
		for _, image := range nsImages {

			if err = cdp.PreDelete(ctx, &cdp.DeleteOption{
				ID:             image.ID,
				ResourceType:   cdp.ResourceDeleteProtectionTypeImage,
				ResourceOrigin: image,
			}); err != nil {
				imagesInUse[image.ID] = &inuseImage{
					ns:    ns,
					Image: &image,
				}
			}

			key := ns + "/" + image.ID
			currentImages.Insert(key)
			record, ok := m.imageRecords[key]
			if !ok {
				record = &imageRecord{
					ns:           ns,
					Image:        &image,
					detectedTime: now,
				}
			} else if _, ok := imagesInUse[key]; ok {
				record.lastUsed = now
			}
			m.imageRecords[key] = record
		}
	}

	for key, image := range m.imageRecords {
		if !currentImages.Has(key) {
			log.G(ctx).Warnf("image %q is no longer present, removing from records", image.ID())
			delete(m.imageRecords, key)
		}
	}

	return imagesInUse, nil
}

func checkImageCosTypeID(labels map[string]string) string {
	if id, ok := labels[constants.LabelContainerImageCosType]; ok {
		return id
	}
	return ""
}

func isContainerInGoodState(c *cubebox.Container) bool {
	if c.GetState() == cubebox.ContainerState_CONTAINER_RUNNING ||
		c.GetState() == cubebox.ContainerState_CONTAINER_PAUSED ||
		c.GetState() == cubebox.ContainerState_CONTAINER_CREATED ||
		c.GetState() == cubebox.ContainerState_CONTAINER_PAUSING {
		return true
	}
	return false
}

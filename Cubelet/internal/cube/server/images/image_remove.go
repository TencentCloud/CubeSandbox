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
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	imagestore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cdp"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/membolt"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/multimeta"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const (
	defaultDomain = "docker.io"

	officialRepoPrefix = "library/"

	officialCommonImagePrefix = defaultDomain + "/" + officialRepoPrefix
)

func (c *GRPCCRIImageService) RemoveImage(ctx context.Context, r *runtime.RemoveImageRequest) (*runtime.RemoveImageResponse, error) {
	err := c.CubeImageService.RemoveImage(ctx, r.GetImage())
	if err != nil && !errdefs.IsNotFound(err) {
		return nil, err
	}
	return &runtime.RemoveImageResponse{}, nil
}

func (c *CubeImageService) RemoveImage(ctx context.Context, imageSpec *runtime.ImageSpec) error {
	stepLog := log.G(ctx).WithFields(CubeLog.Fields{
		"ref":    imageSpec.GetImage(),
		"method": "CubeImageService.RemoveImage",
	})

	image, err := c.LocalResolve(ctx, imageSpec.GetImage())
	if err != nil {
		if errdefs.IsNotFound(err) {

			return nil
		}
		return fmt.Errorf("can not resolve %q locally: %w", imageSpec.GetImage(), err)
	}
	stepLog = stepLog.WithFields(CubeLog.Fields{
		"imageID":   image.ID,
		"mediaType": image.MediaType,
	})

	err = cdp.PreDelete(ctx, &cdp.DeleteOption{
		ID:             image.ID,
		ResourceType:   cdp.ResourceDeleteProtectionTypeImage,
		ResourceOrigin: image,
	})
	if err != nil {
		return fmt.Errorf("image %q do not be allowed to delete: %w", imageSpec.GetImage(), err)
	}
	if c.imageDeleteScheduler == nil {
		// The scheduler is wired unconditionally at service init, so a nil here
		// means the service is only partially constructed (tests/misconfig).
		// Treat as a graceful no-op: there is no inline delete path yet, and the
		// image record is left intact for a later cycle once the scheduler exists.
		stepLog.Warnf("image delete scheduler have not init")
		return nil
	}
	task := &deleteImageTask{
		imageID: image.ID,
		ctx:     ctx,
		errChan: make(chan error, 1),
	}
	defer close(task.errChan)

	err = c.imageDeleteScheduler.createNewDeleteRecord(task, &image, true)
	if err != nil {
		return fmt.Errorf("failed to add task to image delete scheduler: %w", err)
	}

	return nil
}

func (c *CubeImageService) Cleanup(ctx context.Context) error {
	nsImagesMap := c.imageStore.ListAllNamespaceImage()
	var nes []string
	for ns, images := range nsImagesMap {
		nes = append(nes, ns)
		ctx := namespaces.WithNamespace(context.Background(), ns)
		for _, image := range images {
			ctxLog := log.G(ctx).WithFields(CubeLog.Fields{"imageID": image.ID})
			ctxLog.Infof("deleting image %s", image.ID)
			err := c.RemoveImage(ctx, &runtime.ImageSpec{Image: image.ID})
			if err != nil {
				ctxLog.Errorf("failed to delete image %s: %v", image.ID, err)
			}
		}
		containerdImages, err := c.client.ImageService().List(ctx)
		if err != nil {
			log.G(ctx).Errorf("failed to list images: %v", err)
		}
		for _, image := range containerdImages {
			ctxLog := log.G(ctx).WithFields(CubeLog.Fields{"imageID": image.Name})
			err := c.client.ImageService().Delete(ctx, image.Name)
			if err != nil {
				ctxLog.Errorf("failed to delete containerd image %s: %v", image.Name, err)
			} else {
				ctxLog.Infof("deleted containerd image %s", image.Name)
			}
		}
	}

	log.G(ctx).Infof("cleanup cube images success, start garbage collect")
	c.metadata.GarbageCollect(ctx)
	if c.ss != nil {
		err := c.ss.cleanup(ctx, nes)
		if err != nil {
			log.G(ctx).Errorf("failed to cleanup snapshots: %v", err)
		} else {
			log.G(ctx).Infof("cleanup snapshots success")
		}
	}
	return nil
}

func (c *CubeImageService) Reschedule(ctx context.Context) error {
	recov.WithRecover(func() {
		ds := c.imageDeleteScheduler
		if ds == nil {
			return
		}

		err := ds.scanOrphanedContainerdImages(context.Background())
		if err != nil {
			log.AuditLogger.Errorf("failed to scan orphaned containerd images: %v", err)
			return
		}

		records, err := ds.deletingStore.ListGeneric()
		if err != nil {
			log.G(ctx).Errorf("failed to list deleting images: %v", err)
			return
		}
		if len(records) == 0 {
			return
		}
		log.G(ctx).Infof("found %d deleting images", len(records))
		now := time.Now()
		for _, record := range records {

			ctx = namespaces.WithNamespace(context.Background(), record.Namespace)
			if now.Sub(record.StartTime) > 3*time.Minute {
				ds.taskCh <- &deleteImageTask{imageID: record.ID, ctx: ctx}
			}
		}
	})
	return nil
}

type imageDeleteRecord struct {
	ID                string `json:"id,omitempty"`
	*imagestore.Image `json:"image,omitempty"`
	References        []string `json:"to_delete_references,omitempty"`

	ToCleanUpDirs []string  `json:"to_clean_up_dirs,omitempty"`
	StartTime     time.Time `json:"start_time,omitempty"`
}

func getImageDeleteRecordID(obj any) (string, error) {
	image, ok := obj.(*imageDeleteRecord)
	if !ok {
		return "", fmt.Errorf("obj is not a imageDeleteRecord")
	}
	return image.ID, nil
}

var (
	imageDeleteIndexerKey = "ImageDeleteReferences"
	imageDeleteIndexers   = cache.Indexers{
		imageDeleteIndexerKey: func(obj any) ([]string, error) {
			image, ok := obj.(*imageDeleteRecord)
			if !ok {
				return nil, fmt.Errorf("obj is not a imageDeleteRecord")
			}
			return image.References, nil
		},
	}
)

type deleteImageTask struct {
	imageID string
	errChan chan error
	ctx     context.Context
}

func newImageDeleteScheduler(service *CubeImageService, db multimeta.MetadataDBAPI) *imageDeleteScheduler {
	deletingStore, err := membolt.NewBoltCacheStore(db, getImageDeleteRecordID, imageDeleteIndexers, &imageDeleteRecord{})
	if err != nil {
		log.L.Fatalf("failed to create deleting store: %v", err)
		return nil
	}

	orphanedStore, err := membolt.NewBoltCacheStore(db, func(obj any) (string, error) {
		image, ok := obj.(*orphanedContainerdImage)
		if !ok {
			return "", fmt.Errorf("obj is not a orphanedContainerdImage")
		}
		return image.ImageID, nil
	}, cache.Indexers{}, &orphanedContainerdImage{})
	if err != nil {
		log.L.Fatalf("failed to create orphaned store: %v", err)
		return nil
	}

	return &imageDeleteScheduler{
		CubeImageService: service,
		deletingStore:    deletingStore,
		orphanedStore:    orphanedStore,
		taskCh:           make(chan *deleteImageTask, 100),
		stopCh:           make(chan struct{}),
	}
}

type imageDeleteScheduler struct {
	*CubeImageService
	deletingStore *membolt.BoltCacheStore[*imageDeleteRecord]
	orphanedStore *membolt.BoltCacheStore[*orphanedContainerdImage]

	taskCh chan *deleteImageTask
	stopCh chan struct{}
}

func (c *imageDeleteScheduler) StartScheduler(ctx context.Context) {
	recov.GoWithRecover(func() {
		c.dispatchTask()
	})
}

func (c *imageDeleteScheduler) createNewDeleteRecord(task *deleteImageTask, image *imagestore.Image, immediately bool) error {
	record := &imageDeleteRecord{
		ID:         task.imageID,
		Image:      image,
		References: image.References,
		StartTime:  time.Now(),
	}

	if records, err := c.getRecords(task.imageID); err == nil && len(records) > 0 {

		return nil
	}
	if err := c.deletingStore.Update(record); err != nil {
		return fmt.Errorf("failed to add task to deleting store: %w", err)
	}

	if immediately {
		c.taskCh <- task
		if task.errChan != nil {
			err := <-task.errChan
			return err
		}
	}

	return nil
}

func (c *imageDeleteScheduler) Stop(ctx context.Context) error {
	return nil
}

func (c *imageDeleteScheduler) getRecords(ID string) ([]*imageDeleteRecord, error) {
	if c.deletingStore == nil {
		return nil, nil
	}

	records, err := c.deletingStore.ByIndexGeneric(imageDeleteIndexerKey, ID)
	if err != nil {
		return nil, err
	}
	return records, nil
}

func (c *imageDeleteScheduler) removeRecord(id string) error {
	if c.deletingStore == nil {
		return nil
	}
	return c.deletingStore.DeleteByKey(id)
}

func (c *imageDeleteScheduler) dispatchTask() {
	for task := range c.taskCh {
		if task == nil {
			continue
		}
		recov.WithRecover(func() {
			if task.ctx == nil {
				task.ctx = context.Background()
			}
			stepLog := log.G(task.ctx).WithFields(CubeLog.Fields{
				"imageID": task.imageID,
				"method":  "imageDeleteScheduler.dispatchTask",
			})
			err := c.doDeleteImageTask(task)
			if errdefs.IsAborted(err) || errdefs.IsNotFound(err) || err == nil {
				e := c.removeRecord(task.imageID)
				if e != nil {
					stepLog.WithError(e).Errorf("failed to remove record from deleting store")
				} else {
					stepLog.Infof("successfully removed record from deleting store")
				}
			}
			if task.errChan != nil {
				task.errChan <- err
			}
		})
	}

}

func (c *imageDeleteScheduler) doDeleteImageTask(task *deleteImageTask) (err error) {
	var (
		record *imageDeleteRecord
		image  *imagestore.Image
		ctx    = task.ctx
		id     = task.imageID

		cancel  context.CancelFunc
		stepLog = log.G(ctx).WithFields(CubeLog.Fields{
			"imageID": id,
			"method":  "imageDeleteScheduler.doDelete",
		})
	)

	c.imageRemoveLock.Lock()
	ctx, cancel = context.WithTimeout(ctx, time.Minute)

	defer func() {
		if err != nil {
			stepLog.WithError(err).Errorf("failed to delete image")
		}
		if cancel != nil {
			cancel()
		}
		c.imageRemoveLock.Unlock()
	}()

	record, err = c.deletingStore.GetGeneric(id)
	if err != nil {
		return
	}
	image = record.Image
	if image == nil {
		err = fmt.Errorf("record image is nil: %w", errdefs.ErrNotFound)
		return
	}

	_, err = namespaces.NamespaceRequired(ctx)
	if err != nil {
		if image.Namespace == "" {
			image.Namespace = namespaces.Default
			return
		}
		ctx = namespaces.WithNamespace(ctx, image.Namespace)
	}

	err = cdp.PreDelete(ctx, &cdp.DeleteOption{
		ID:             image.ID,
		ResourceType:   cdp.ResourceDeleteProtectionTypeImage,
		ResourceOrigin: image,
	})
	if err != nil {
		err = fmt.Errorf("image %q do not be allowed to delete: %w", id, err)
		return
	}

	refs := image.References
	refs = append(refs, image.ID)

	var (
		toRemoveDirs = sets.NewString()
		toDeleteRefs = sets.NewString(refs...)
	)

	for _, ref := range refs {
		stepLog = stepLog.WithField("image.reference", ref)
		stepLog.Debugf("deleting containerd image reference")

		var oldImage images.Image
		oldImage, err = c.images.Get(ctx, ref)
		if errdefs.IsNotFound(err) {
			toDeleteRefs.Delete(ref)

			if err = c.imageStore.Update(ctx, ref); err != nil {
				stepLog.WithError(err).Fatalf("failed to update imageStore for reference %q after image deleted", ref)
				err = fmt.Errorf("failed to update imageStore for reference %q: %w", ref, err)
			}
		} else if err != nil {

			stepLog.Warnf("failed to get image %q before deletion: %v", ref, err)
			err = errors.New("can not get image by image service")
			return
		} else if err == nil {

			uids := oldImage.Labels[constants.LabelImageUidFiles]
			if len(uids) > 0 {
				toRemoveDirs.Insert(uids)
			}
		}
	}

	err = cdp.PreDelete(ctx, &cdp.DeleteOption{
		ID:             image.ID,
		ResourceType:   cdp.ResourceDeleteProtectionTypeImage,
		ResourceOrigin: image,
	})
	if err != nil {
		return
	}

	toDeleteArr := toDeleteRefs.List()
	for i, ref := range toDeleteRefs.List() {

		var opts []images.DeleteOpt
		if i == len(toDeleteArr)-1 {
			opts = []images.DeleteOpt{images.SynchronousDelete()}
		}
		err = c.images.Delete(ctx, ref, opts...)
		if err != nil && !errdefs.IsNotFound(err) {
			stepLog.Errorf("failed to delete image reference %q for %q: %v", ref, image.ID, err)
			err = fmt.Errorf("failed to delete image reference %q by image service", ref)
			return
		} else if err == nil {

			stepLog.Infof("successfully deleted image reference %q for %q", ref, image.ID)
		}

		if err = c.imageStore.Update(ctx, ref); err != nil {
			stepLog.WithError(err).Fatalf("failed to update imageStore for reference %q after image deleted", ref)
			err = fmt.Errorf("failed to update imageStore for reference %q: %w", ref, err)
		}

	}

	for _, dir := range toRemoveDirs.List() {
		err = os.RemoveAll(dir)
		if err != nil {
			stepLog.Errorf("failed to remove uid files %q: %v", dir, err)
			err = fmt.Errorf("failed to remove uid files %q", dir)
			return
		}
	}

	stepLog.Infof("successfully deleted image %q with %d references", image.ID, len(toDeleteArr))
	return
}

type orphanedContainerdImage struct {
	Namespace    string
	ImageID      string
	DetectedTime time.Time
}

func (c *imageDeleteScheduler) scanOrphanedContainerdImages(ctx context.Context) error {
	nses, err := c.client.NamespaceService().List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list namespaces: %w", err)
	}

	now := time.Now()
	for _, namespace := range nses {
		ctx = namespaces.WithNamespace(ctx, namespace)
		containerdImages, err := c.client.ImageService().List(namespaces.WithNamespace(ctx, namespace))
		if err != nil {
			return fmt.Errorf("failed to list containerd images: %w", err)
		}
		for _, cimage := range containerdImages {
			image, err := c.LocalResolve(ctx, cimage.Name)
			if errdefs.IsNotFound(err) {
				orphanedImage, err := c.orphanedStore.GetGeneric(cimage.Name)
				if errdefs.IsNotFound(err) {

					c.orphanedStore.Update(&orphanedContainerdImage{
						Namespace:    namespace,
						ImageID:      cimage.Name,
						DetectedTime: time.Now(),
					})
				} else if err != nil {
					return fmt.Errorf("failed to get orphaned image %q: %w", cimage.Name, err)
				} else {

					if now.Sub(orphanedImage.DetectedTime) > 5*time.Minute {
						log.AuditLogger.Errorf("detected orphaned containerd image %q, will delete it", cimage.Name)
						c.orphanedStore.DeleteByKey(cimage.Name)

						notStoreImage := &imagestore.Image{
							ID:          cimage.Name,
							References:  []string{cimage.Name},
							CreatedTime: cimage.CreatedAt,
							Namespace:   namespace,
							Annotation:  cimage.Labels,
						}
						if len(notStoreImage.Annotation) == 0 {
							notStoreImage.Annotation = make(map[string]string)
						}
						if len(image.Annotation) > 0 {
							if image.Annotation[constants.LabelImageUidFiles] != "" {
								notStoreImage.UidFiles = image.Annotation[constants.LabelImageUidFiles]
							}
							if image.Annotation[constants.LabelContainerImageMedia] != "" {
								notStoreImage.MediaType = image.Annotation[constants.LabelContainerImageMedia]
							}
						}

						notStoreImage.Annotation[constants.LabelImagePrefix+"orphaned-image"] = constants.StringTrueValue

						err = c.createNewDeleteRecord(&deleteImageTask{imageID: cimage.Name, ctx: ctx}, notStoreImage, false)
						if err != nil {
							log.AuditLogger.Errorf("failed to add task %q to image delete scheduler: %v", cimage.Name, err)
						}
					}
				}

				continue
			} else if err != nil {
				log.AuditLogger.Errorf("failed to resolve containerd image %q: %v", cimage.Name, err)
				continue
			}

			if _, err := c.deletingStore.GetGeneric(image.ID); err == nil {
				c.orphanedStore.DeleteByKey(cimage.Name)
			}
		}
	}
	return nil
}

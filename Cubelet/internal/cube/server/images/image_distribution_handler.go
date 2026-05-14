// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package images

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	cubeimages "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/server/images/ext4image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/nodedistribution/distribution"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/recov"
	"github.com/tencentcloud/CubeSandbox/cubelog"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func afterImageRecoverForHandler(c *CubeImageService) {
	distribution.RegisterHandler(distribution.ResourceTaskTypeImage, &imageDistributionHandler{
		CubeImageService: c,
	})

	if c.imageDeleteScheduler != nil {
		c.imageDeleteScheduler.StartScheduler(context.Background())
	}
}

type imageDistributionHandler struct {
	*CubeImageService
}

func defaultTemplateImageSpec(ns string, template *templatetypes.TemplateImage) *cubeimages.ImageSpec {
	annotations := map[string]string{}
	var (
		storageMedia string
		imageRef     string
	)
	if template != nil {
		storageMedia = template.StorageMedia
		imageRef = template.Image
		if template.StorageMedia == cubeimages.ImageStorageMediaType_ext4.String() {
			annotations[constants.MasterAnnotationInstanceType] = cubebox.InstanceType_cubebox.String()
		}
	}
	return &cubeimages.ImageSpec{
		StorageMedia: storageMedia,
		Image:        imageRef,
		Namespace:    ns,
		Annotations:  annotations,
	}
}

func materializeDistributedTemplateRuntimeFiles(ctx context.Context, template *templatetypes.TemplateImage) error {
	if template == nil || template.StorageMedia != cubeimages.ImageStorageMediaType_ext4.String() {
		return nil
	}
	return ext4image.RefreshArtifactRuntimeFiles(ctx, cubebox.InstanceType_cubebox.String(), template.Image)
}

func ensureDistributedTemplateImage(ctx context.Context, c *CubeImageService, template *templatetypes.TemplateImage) error {
	if template == nil {
		return fmt.Errorf("template image is nil")
	}
	if template.StorageMedia == cubeimages.ImageStorageMediaType_ext4.String() {
		return ext4image.EnsurePmemRootfs(ctx, cubebox.InstanceType_cubebox.String(), template.Image)
	}
	_, err := c.EnsureImage(ctx, template.Image, "", "", &runtime.PodSandboxConfig{})
	return err
}

func (c *imageDistributionHandler) GetTaskExternObjByKey(ctx context.Context, key string) (any, error) {
	return c.GetImage(ctx, key)
}

func (c *imageDistributionHandler) Handle(ctx context.Context, task *distribution.SubTaskDefine) (status distribution.TaskStatus, err error) {
	imageStatus := newImageTaskStatus(task)
	status = imageStatus

	logEntry := log.G(ctx).WithFields(CubeLog.Fields{
		"method":      "imageDistributionHandler.Handle",
		"task_id":     task.DistributionTaskID,
		"template_id": task.TemplateID,
		"namespace":   task.Namespace,
		"task_type":   task.Type,
	})

	defer func() {
		recov.HandleCrash(func(panicError interface{}) {
			err = fmt.Errorf("image distribution handler failed with panic: %v", panicError)
		})
		if err != nil {
			logEntry.Error(err)
			status.AddError(ctx, err)
		} else {
			imageStatus.SetStatus(distribution.TaskStatus_SUCCESS, "")
			logEntry.Infof("image distribution handler success")
		}
	}()

	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		err = fmt.Errorf("get namespace failed: %w", err)
		return
	}

	switch task.Type {
	case distribution.ResourceTaskTypeImage:
		template, ok := task.Object.(*templatetypes.TemplateImage)
		if !ok {
			err = fmt.Errorf("invalid template image object type: %T", task.Object)
			return
		}

		if template.Image == "" {
			err = fmt.Errorf("template image is empty")
			return
		}

		logEntry = logEntry.WithFields(CubeLog.Fields{
			"image":         template.Image,
			"storage_media": template.StorageMedia,
		})
		ctx = log.WithLogger(ctx, logEntry)

		cubeSpec := defaultTemplateImageSpec(ns, template)
		ctx = constants.WithImageSpec(ctx, cubeSpec)

		logEntry.Infof("ensuring image: %s", template.Image)
		if err = ensureDistributedTemplateImage(ctx, c.CubeImageService, template); err != nil {
			err = fmt.Errorf("ensure image failed: %w", err)
			return
		}
		if err = materializeDistributedTemplateRuntimeFiles(ctx, template); err != nil {
			err = fmt.Errorf("materialize template runtime files failed: %w", err)
			return
		}

		var img image.Image
		img, err = c.GetImage(ctx, template.Image)
		if err != nil {
			err = fmt.Errorf("get image failed: %w", err)
			return
		}

		imageStatus.LocalDistributionImage = &templatetypes.LocalDistributionImage{
			DistributionReference: *task.GenDistributionReference(),
			TemplateImage: templatetypes.TemplateImage{
				Name:         template.Name,
				Namespace:    ns,
				Image:        template.Image,
				StorageMedia: string(template.StorageMedia),
			},
			Image: img,
		}

		logEntry.WithFields(CubeLog.Fields{
			"image_id":   img.ID,
			"image_size": img.Size,
		}).Infof("image ensured successfully")

		return

	default:
		err = fmt.Errorf("unsupported task type: %v", task.Type)
		return
	}
}

func (c *imageDistributionHandler) IsReady() bool {
	return true
}

var _ distribution.TaskHandler = &imageDistributionHandler{}

type ImageTaskStatus struct {
	*distribution.BaseSubTaskStatus
	LocalDistributionImage *templatetypes.LocalDistributionImage
}

func newImageTaskStatus(task *distribution.SubTaskDefine) *ImageTaskStatus {
	return &ImageTaskStatus{
		BaseSubTaskStatus: task.NewRunningStatus(),
	}
}

func (i *ImageTaskStatus) GetExternObj() any {
	return i.LocalDistributionImage
}

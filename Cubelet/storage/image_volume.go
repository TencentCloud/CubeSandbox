// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow"
	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
)

func (l *local) prepareImageVolume(ctx context.Context, opts *workflow.CreateContext, v *cubebox.Volume, result *StorageInfo) error {
	if v.GetVolumeSource() == nil || v.GetVolumeSource().GetImage() == nil {
		return nil
	}
	var (
		imageVolumeSource = v.GetVolumeSource().GetImage()
		imageSpec         = imageVolumeSource.GetReference()
		start             = time.Now()
	)

	defer func() {
		workflow.RecordCreateMetricIfGreaterThan(ctx, nil, "storage.prepareImageVolume", time.Since(start), time.Millisecond)
	}()

	logEntry := log.G(ctx).WithFields(CubeLog.Fields{
		"method": "local.prepareImageVolume",
		"name":   v.GetName(),
		"image":  imageSpec.GetImage(),
	})

	_ = result
	logEntry.WithField("image", imageSpec.GetImage()).Warn("image volume based on erofs is unsupported")
	return fmt.Errorf("image volume is not supported in the open source build")
}

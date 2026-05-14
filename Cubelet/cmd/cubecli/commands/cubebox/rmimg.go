// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	gocontext "context"
	"log"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	refdocker "github.com/distribution/reference"
	"github.com/urfave/cli/v2"
)

var (
	cntdClient *containerd.Client
)

func removeImage(context *cli.Context, imageRef string) error {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("removeImage panic: %+v", r)
			return
		}
	}()
	cntCtx := namespaces.WithNamespace(gocontext.Background(), context.String("namespace"))
	cntCtx, cntCancel := gocontext.WithTimeout(cntCtx, context.Duration("timeout"))
	defer cntCancel()

	named, err := refdocker.ParseDockerRef(imageRef)
	if err != nil {
		log.Printf("failed to parse image ref: %+v", err)
		return err
	}
	ref := named.String()
	if img, err := cntdClient.ImageService().Get(cntCtx, ref); err == nil {
		image := containerd.NewImage(cntdClient, img)
		err := cntdClient.ImageService().Delete(cntCtx, image.Name(), images.SynchronousDelete())
		if err != nil {
			return err
		}
		log.Printf("image %q remove succ", ref)
	} else {
		log.Printf("no such image %q", ref)
	}
	return nil
}

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"encoding/json"
	"github.com/sirupsen/logrus"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	cubeboxstore "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/store/cubebox"
	"github.com/urfave/cli/v2"
	errorUtils "k8s.io/apimachinery/pkg/util/errors"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

var removeImageCommand = &cli.Command{
	Name:                   "rmi",
	Usage:                  "Remove one or more images",
	ArgsUsage:              "IMAGE-ID [IMAGE-ID...]",
	UseShortOptionHandling: true,
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "all",
			Aliases: []string{"a"},
			Usage:   "Remove all images",
		},
		&cli.BoolFlag{
			Name:    "prune",
			Aliases: []string{"q"},
			Usage:   "Remove all unused images",
		},
	},
	Action: func(cliCtx *cli.Context) error {
		conn, ctx, cancel, err := commands.NewGrpcConn(cliCtx)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()
		imageClient := runtime.NewImageServiceClient(conn)

		ids := map[string]bool{}
		for _, id := range cliCtx.Args().Slice() {
			logrus.Debugf("User specified image to be removed: %v", id)
			ids[id] = true
		}

		all := cliCtx.Bool("all")
		prune := cliCtx.Bool("prune")

		if all || prune {
			r, err := ListImages(ctx, imageClient, "", nil)
			if err != nil {
				return err
			}
			for _, img := range r.Images {

				if prune && img.Pinned {
					logrus.Debugf("Excluding pinned container image: %v", img.GetId())

					continue
				}
				logrus.Debugf("Adding container image to be removed: %v", img.GetId())
				ids[img.GetId()] = true
			}
		}

		if prune {
			cubeboxClient := cubebox.NewCubeboxMgrClient(conn)
			req := &cubebox.ListCubeSandboxRequest{
				Option: &cubebox.ListCubeSandboxOption{
					PrivateWithCubeboxStore: true,
				},
			}
			resp, err := cubeboxClient.List(ctx, req)
			if err != nil {
				return fmt.Errorf("failed to list cube sandbox: %w", err)
			}
			for _, box := range resp.GetItems() {
				cubeBox := &cubeboxstore.CubeBox{}
				err := json.Unmarshal(box.PrivateCubeboxStorageData, cubeBox)
				if err != nil {
					return fmt.Errorf("failed to unmarshal cubebox for %s: %w", box.Id, err)
				}

				var hostedImage = []string{}
				hostedImage = append(hostedImage, cubeBox.GetOrCreatePodConfig().GetHostedImageList()...)
				for _, container := range box.GetContainers() {
					hostedImage = append(hostedImage, container.GetImage())
				}
				for _, img := range hostedImage {
					imageStatus, err := ImageStatus(ctx, imageClient, img, false)
					if err != nil {
						logrus.Errorf(
							"image status request for %q failed: %v",
							img, err,
						)

						continue
					}
					id := imageStatus.GetImage().GetId()
					logrus.Debugf("Excluding in use container image: %v", id)
					ids[id] = false
				}
			}
		}

		if len(ids) == 0 {
			if all || prune {
				logrus.Info("No images to remove")

				return nil
			}

			return cli.ShowSubcommandHelp(cliCtx)
		}

		funcs := []func() error{}
		for id, remove := range ids {
			if !remove {
				continue
			}
			funcs = append(funcs, func() error {
				status, err := ImageStatus(ctx, imageClient, id, false)
				if err != nil {
					return fmt.Errorf("image status request for %q failed: %w", id, err)
				}
				if status.Image == nil {
					return fmt.Errorf("no such image %s", id)
				}

				if err := RemoveImage(ctx, imageClient, id); err != nil {

					if !prune {
						return fmt.Errorf("error of removing image %q: %w", id, err)
					}

					return nil
				}
				if len(status.Image.RepoTags) == 0 {

					for _, repoDigest := range status.Image.RepoDigests {
						fmt.Printf("Deleted: %s\n", repoDigest)
					}

					return nil
				}
				for _, repoTag := range status.Image.RepoTags {
					fmt.Printf("Deleted: %s\n", repoTag)
				}

				return nil
			})
		}

		return errorUtils.AggregateGoroutines(funcs...)
	},
}

func RemoveImage(ctx context.Context, client runtime.ImageServiceClient, image string) error {
	if image == "" {
		return errors.New("ImageID cannot be empty")
	}
	request := &runtime.RemoveImageRequest{Image: &runtime.ImageSpec{Image: image}}
	logrus.Debugf("RemoveImageRequest: %v", request)

	_, err := client.RemoveImage(ctx, request)
	return err
}

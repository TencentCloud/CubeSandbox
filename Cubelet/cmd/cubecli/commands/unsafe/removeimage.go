// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package unsafe

import (
	"fmt"
	"log"

	ctrCommands "github.com/containerd/containerd/v2/cmd/ctr/commands"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
	"github.com/urfave/cli/v2"
)

var RemoveImage = &cli.Command{
	Name:  "rmi",
	Usage: "remove images",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:    "all",
			Aliases: []string{"a"},
			Usage:   "remove all images",
		},
	},
	Action: func(cliContext *cli.Context) error {
		var toRemoveNamespaceImages map[string][]string = make(map[string][]string)
		if cliContext.Bool("all") {
			client, ctx, cancel, err := ctrCommands.NewClient(cliContext)
			if err != nil {
				return err
			}
			defer cancel()
			var (
				imageStore = client.ImageService()
			)
			nslist, err := client.NamespaceService().List(ctx)
			if err != nil {
				return fmt.Errorf("failed to list namespaces: %w", err)
			}
			for _, ns := range nslist {
				var toRemoveImage []string
				imageList, err := imageStore.List(namespaces.WithNamespace(ctx, ns))
				if err != nil {
					return fmt.Errorf("failed to list images: %w", err)
				}
				for _, image := range imageList {
					toRemoveImage = append(toRemoveImage, image.Name)
				}
				toRemoveNamespaceImages[ns] = toRemoveImage
			}
		} else {
			toRemoveNamespaceImages[cliContext.String("namespace")] = cliContext.Args().Slice()
			if len(toRemoveNamespaceImages[cliContext.String("namespace")]) == 0 {
				return fmt.Errorf("image id must be provided: %w", errdefs.ErrInvalidArgument)
			}
		}

		if !commands.AskForConfirm("will destroy image, continue only if you confirm", 3) {
			return nil
		}

		conn, ctx, cancel, err := commands.NewGrpcConn(cliContext)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()

		client := images.NewImagesClient(conn)
		for ns, toRemoveImages := range toRemoveNamespaceImages {
			for _, imageName := range toRemoveImages {
				resp, err := client.DestroyImage(ctx, &images.DestroyImageRequest{
					Spec: &images.ImageSpec{
						Annotations: map[string]string{
							constants.AnnotationCubeletNameSpace: ns,
						},
						Image: imageName,
					},
				})

				if err != nil {
					return err
				}
				if !ret.IsSuccessCode(resp.GetRet().GetRetCode()) {
					return fmt.Errorf("destroy image %q: %v", imageName, resp.GetRet().GetRetMsg())
				}
				log.Printf("destroy image %q %v", imageName, resp.GetRet().GetRetMsg())
			}
		}

		return nil
	},
}

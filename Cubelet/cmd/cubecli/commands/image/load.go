// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	gocontext "context"
	"errors"
	"fmt"
	"log"
	"os"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/platforms"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/urfave/cli/v2"
)

var Load = &cli.Command{
	Name:  "load",
	Usage: "Warning: load image is deprecated, `cubecli image pull` instead",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "snapshotter",
			Value: "overlayfs",
			Usage: "snapshotter format",
		},
		&cli.StringFlag{
			Name:    "input",
			Aliases: []string{"i"},
			Value:   "",
			Usage:   "Read from tar archive file, instead of STDIN",
		},
	},
	Action: func(context *cli.Context) error {
		input := context.String("input")
		if input == "" {
			log.Printf("should provide input")
			return nil
		}

		f, err := os.Open(input)
		if err != nil {
			return err
		}
		defer f.Close()

		platMC := platforms.DefaultStrict()
		cntdClient, err := containerd.New(context.String("address"),
			containerd.WithDefaultPlatform(platMC))
		if err != nil {
			return fmt.Errorf("init containerd connect failed.%s", err)
		}
		cntCtx := namespaces.WithNamespace(gocontext.Background(), context.String("namespace"))
		cntCtx, cntCancel := gocontext.WithTimeout(cntCtx, context.Duration("timeout"))
		defer cntCancel()

		imgs, err := cntdClient.Import(cntCtx, f,
			containerd.WithDigestRef(archive.DigestTranslator("cube-import/"+context.String("snapshotter"))),
			containerd.WithSkipDigestRef(func(name string) bool { return name != "" }),
			containerd.WithImportPlatform(platMC),
			containerd.WithImageLabels(map[string]string{
				constants.LabelImageCreateBy: constants.LabelImageCreateByImport,
			}))

		if err != nil {
			if errors.Is(err, images.ErrEmptyWalk) {
				err = fmt.Errorf("%w (Hint: set `--platform=PLATFORM` or `--all-platforms`)", err)
			}
			return err
		}
		for _, img := range imgs {
			image := containerd.NewImageWithPlatform(cntdClient, img, platMC)

			fmt.Fprintf(os.Stdout, "unpacking %s (%s)...", img.Name, img.Target.Digest)
			err = image.Unpack(cntCtx, context.String("snapshotter"))
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stdout, "done\n")
		}

		return nil
	},
}

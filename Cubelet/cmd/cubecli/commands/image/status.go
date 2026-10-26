// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"context"
	"fmt"

	"github.com/docker/go-units"
	"github.com/sirupsen/logrus"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/urfave/cli/v2"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

var imageStatusCommand = &cli.Command{
	Name:                   "inspecti",
	Usage:                  "Return the status of one or more images",
	ArgsUsage:              "IMAGE-ID [IMAGE-ID...]",
	UseShortOptionHandling: true,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "output",
			Aliases: []string{"o"},
			Usage:   "Output format, One of: json|yaml|go-template|table",
		},
		&cli.BoolFlag{
			Name:    "quiet",
			Aliases: []string{"q"},
			Usage:   "Do not show verbose information",
		},
		&cli.StringFlag{
			Name:  "template",
			Usage: "The template string is only used when output is go-template; The Template format is golang template",
		},
		&cli.StringFlag{
			Name:  "name",
			Usage: "Filter by image name",
		},
		&cli.StringSliceFlag{
			Name:    "filter",
			Aliases: []string{"f"},
			Usage:   "Filter output based on provided conditions.\nAvailable filters: \n* dangling=(boolean - true or false)\n* reference=/regular expression/\n* before=<image-name>[:<tag>]|<image id>|<image@digest>\n* since=<image-name>[:<tag>]|<image id>|<image@digest>\nMultiple filters can be combined together.",
		},
	},
	Action: func(c *cli.Context) error {
		conn, ctx, cancel, err := commands.NewGrpcConn(c)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()
		imageClient := runtime.NewImageServiceClient(conn)
		verbose := !(c.Bool("quiet"))
		output := c.String("output")
		if output == "" {
			output = outputTypeJSON
		}
		tmplStr := c.String("template")

		ids := c.Args().Slice()

		if len(ids) == 0 {
			r, err := ListImages(ctx, imageClient, c.String("name"), c.StringSlice("filter"))
			if err != nil {
				return fmt.Errorf("listing images: %w", err)
			}
			for _, img := range r.GetImages() {
				ids = append(ids, img.GetId())
			}
		}

		if len(ids) == 0 {
			logrus.Error("No IDs provided or nothing found per filter")

			return cli.ShowSubcommandHelp(c)
		}

		statuses := []statusData{}
		for _, id := range ids {
			r, err := ImageStatus(ctx, imageClient, id, verbose)
			if err != nil {
				return fmt.Errorf("image status for %q request: %w", id, err)
			}

			if r.Image == nil {
				return fmt.Errorf("no such image %q present", id)
			}

			statusJSON, err := protobufObjectToJSON(r.Image)
			if err != nil {
				return fmt.Errorf("marshal status to JSON for %q: %w", id, err)
			}

			if output == outputTypeTable {
				outputImageStatusTable(r, verbose)
			} else {
				statuses = append(statuses, statusData{json: statusJSON, info: r.Info})
			}
		}

		return outputStatusData(statuses, output, tmplStr)
	},
}

func ImageStatus(ctx context.Context, client runtime.ImageServiceClient, image string, verbose bool) (*runtime.ImageStatusResponse, error) {
	request := &runtime.ImageStatusRequest{
		Image:   &runtime.ImageSpec{Image: image},
		Verbose: verbose,
	}
	logrus.Debugf("ImageStatusRequest: %v", request)

	res, err := client.ImageStatus(ctx, request)
	if err != nil {
		return nil, err
	}

	logrus.Debugf("ImageStatusResponse: %v", res)

	return res, nil
}

func outputImageStatusTable(r *runtime.ImageStatusResponse, verbose bool) {

	fmt.Printf("ID: %s\n", r.Image.Id)

	for _, tag := range r.Image.RepoTags {
		fmt.Printf("Tag: %s\n", tag)
	}

	for _, digest := range r.Image.RepoDigests {
		fmt.Printf("Digest: %s\n", digest)
	}

	size := units.HumanSizeWithPrecision(float64(r.Image.GetSize_()), 3)
	fmt.Printf("Size: %s\n", size)

	if verbose {
		fmt.Printf("Info: %v\n", r.GetInfo())
	}
}

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

var imageFsInfoCommand = &cli.Command{
	Name:                   "imagefsinfo",
	Usage:                  "Return image filesystem info",
	UseShortOptionHandling: true,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "output",
			Aliases: []string{"o"},
			Usage:   "Output format, One of: json|yaml|go-template|table",
		},
		&cli.StringFlag{
			Name:  "template",
			Usage: "The template string is only used when output is go-template; The Template format is golang template",
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

		output := c.String("output")
		if output == "" {
			output = outputTypeJSON
		}
		tmplStr := c.String("template")

		r, err := ImageFsInfo(ctx, imageClient)
		if err != nil {
			return fmt.Errorf("image filesystem info request: %w", err)
		}
		status, err := protobufObjectToJSON(r)
		if err != nil {
			return fmt.Errorf("marshal filesystem info to json: %w", err)
		}

		if output == outputTypeTable {
			outputImageFsInfoTable(r)
		} else {
			return outputStatusData([]statusData{{json: status}}, output, tmplStr)
		}

		return nil
	},
}

func outputImageFsInfoTable(r *runtime.ImageFsInfoResponse) {
	tablePrintFileSystem := func(fileLabel string, filesystem []*runtime.FilesystemUsage) {
		fmt.Printf("%s Filesystem \n", fileLabel)

		for i, val := range filesystem {
			fmt.Printf("TimeStamp[%d]: %d\n", i, val.Timestamp)
			fmt.Printf("Disk[%d]: %s\n", i, units.HumanSize(float64(val.UsedBytes.GetValue())))
			fmt.Printf("Inodes[%d]: %d\n", i, val.InodesUsed.GetValue())
			fmt.Printf("Mountpoint[%d]: %s\n", i, val.FsId.Mountpoint)
		}
	}

	tablePrintFileSystem("Image", r.ImageFilesystems)
}

func ImageFsInfo(ctx context.Context, client runtime.ImageServiceClient) (*runtime.ImageFsInfoResponse, error) {
	resp, err := client.ImageFsInfo(ctx, &runtime.ImageFsInfoRequest{})
	if err != nil {
		return nil, err
	}
	logrus.Debugf("ImageFsInfoResponse: %v", resp)

	return resp, nil
}

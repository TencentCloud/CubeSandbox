// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	jsoniter "github.com/json-iterator/go"
	"github.com/urfave/cli/v2"
	"google.golang.org/grpc/status"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
)

var lsdb = &cli.Command{
	Name:  "ls",
	Usage: "list sandbox storage volumes via cubelet gRPC",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "bucket",
			Aliases: []string{"b"},
			Value:   "emptydir/v1",
			Usage:   "bucket name",
		},
		&cli.BoolFlag{
			Name:  "raw",
			Usage: "raw json (per sandbox)",
		},
		&cli.BoolFlag{
			Name:  "each-raw",
			Usage: "append raw volume struct per row",
		},
	},
	Action: func(cliCtx *cli.Context) error {
		conn, grpcCtx, cancel, err := commands.NewGrpcConn(cliCtx)
		if err != nil {
			return fmt.Errorf("cubelet must be running to inspect storage; start cubelet first: %w", err)
		}
		defer conn.Close()
		defer cancel()

		client := cubebox.NewCubeboxMgrClient(conn)
		grpcCtx, grpcCancel := context.WithTimeout(grpcCtx, cliCtx.Duration("timeout"))
		defer grpcCancel()

		resp, err := client.InspectStorageVolumes(grpcCtx, &cubebox.InspectStorageVolumesRequest{
			Bucket: cliCtx.String("bucket"),
		})
		if err != nil {
			if st, ok := status.FromError(err); ok && st.Code().String() == "Unavailable" {
				return fmt.Errorf("cubelet must be running to inspect storage; start cubelet first")
			}
			return fmt.Errorf("InspectStorageVolumes failed: %w", err)
		}
		if ret := resp.GetRet(); ret == nil || ret.GetRetCode() != errorcode.ErrorCode_Success {
			msg := "<empty>"
			if ret != nil {
				msg = ret.GetRetMsg()
			}
			return fmt.Errorf("InspectStorageVolumes returned error: %s", msg)
		}

		if cliCtx.Bool("raw") {
			for _, sb := range resp.GetSandboxes() {
				out, _ := jsoniter.MarshalToString(sb)
				fmt.Printf("%s\t%s\n", sb.GetSandboxID(), out)
			}
			return nil
		}
		return printStorageRows(os.Stdout, resp.GetSandboxes(), cliCtx.Bool("each-raw"))
	},
}

func printStorageRows(w io.Writer, sandboxes []*cubebox.SandboxStorageInfo, eachRaw bool) error {
	tw := tabwriter.NewWriter(w, 4, 8, 4, ' ', 0)
	tabHeader := "NS\tID\tName\tFile\tSize\tCowVol\tKind\tGen"
	if eachRaw {
		tabHeader += "\tRAW"
	}
	if _, err := fmt.Fprintln(tw, tabHeader); err != nil {
		return err
	}
	for _, sb := range sandboxes {
		if sb == nil {
			continue
		}
		for _, volume := range sb.GetVolumes() {
			if volume == nil {
				continue
			}
			row := fmt.Sprintf("%s\t%s\t%s\t%s\t%d\t%s\t%s\t%d",
				sb.GetNamespace(),
				sb.GetSandboxID(),
				volume.GetName(),
				volume.GetFilePath(),
				volume.GetSizeLimit(),
				volume.GetVolumeName(),
				volume.GetKind(),
				volume.GetGen(),
			)
			if eachRaw {
				raw, _ := jsoniter.MarshalToString(volume)
				row += fmt.Sprintf("\t%s", raw)
			}
			if _, err := fmt.Fprintln(tw, row); err != nil {
				return err
			}
		}
	}
	return tw.Flush()
}

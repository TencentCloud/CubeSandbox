// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/urfave/cli/v2"
	"google.golang.org/grpc/status"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	cubecommands "github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
)

var cleanup = &cli.Command{
	Name:  "cleanup",
	Usage: "cleanup orphan emptydir files via cubelet gRPC",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "bucket",
			Aliases: []string{"b"},
			Value:   "emptydir/v1",
			Usage:   "bucket name",
		},
		&cli.StringSliceFlag{
			Name:  "format",
			Usage: "format roots to scan, e.g. \"512Mi\", \"1Gi\", \"others\". When omitted cubelet uses its default list.",
		},
		&cli.BoolFlag{
			Name:  "dry-run",
			Usage: "list orphans but do not delete",
		},
	},
	Action: func(cliCtx *cli.Context) error {
		conn, grpcCtx, cancel, err := cubecommands.NewGrpcConn(cliCtx)
		if err != nil {
			return fmt.Errorf("cubelet must be running to cleanup storage; start cubelet first: %w", err)
		}
		defer conn.Close()
		defer cancel()

		client := cubebox.NewCubeboxMgrClient(conn)
		grpcCtx, grpcCancel := context.WithTimeout(grpcCtx, cliCtx.Duration("timeout"))
		defer grpcCancel()

		dryRun := cliCtx.Bool("dry-run")
		previewReq := &cubebox.CleanupOrphanStorageFilesRequest{
			Bucket:  cliCtx.String("bucket"),
			Formats: cliCtx.StringSlice("format"),
			DryRun:  true,
		}
		previewResp, err := client.CleanupOrphanStorageFiles(grpcCtx, previewReq)
		if err != nil {
			if st, ok := status.FromError(err); ok && st.Code().String() == "Unavailable" {
				return fmt.Errorf("cubelet must be running to cleanup storage; start cubelet first")
			}
			return fmt.Errorf("CleanupOrphanStorageFiles preview failed: %w", err)
		}
		if ret := previewResp.GetRet(); ret == nil || ret.GetRetCode() != errorcode.ErrorCode_Success {
			msg := "<empty>"
			if ret != nil {
				msg = ret.GetRetMsg()
			}
			return fmt.Errorf("CleanupOrphanStorageFiles returned error: %s", msg)
		}

		w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
		fmt.Fprintln(w, "Format\tFile\tStatus")
		orphans := previewResp.GetOrphans()
		for _, e := range orphans {
			fmt.Fprintf(w, "%s\t%s\torphan\n", e.GetFormat(), e.GetFilePath())
		}
		w.Flush()

		if dryRun {
			return nil
		}
		if len(orphans) == 0 {
			return nil
		}
		if !cubecommands.AskForConfirm("cleanup will destroy ALL of the resources above, continue only if you confirm", 3) {
			return nil
		}

		actionReq := &cubebox.CleanupOrphanStorageFilesRequest{
			Bucket:  cliCtx.String("bucket"),
			Formats: cliCtx.StringSlice("format"),
			DryRun:  false,
		}
		actionResp, err := client.CleanupOrphanStorageFiles(grpcCtx, actionReq)
		if err != nil {
			return fmt.Errorf("CleanupOrphanStorageFiles failed: %w", err)
		}
		if ret := actionResp.GetRet(); ret == nil || ret.GetRetCode() != errorcode.ErrorCode_Success {
			msg := "<empty>"
			if ret != nil {
				msg = ret.GetRetMsg()
			}
			return fmt.Errorf("CleanupOrphanStorageFiles returned error: %s", msg)
		}

		w2 := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
		fmt.Fprintln(w2, "Format\tFile\tRemoved\tError")
		for _, e := range actionResp.GetOrphans() {
			fmt.Fprintf(w2, "%s\t%s\t%v\t%s\n", e.GetFormat(), e.GetFilePath(), e.GetRemoved(), e.GetErrorMessage())
		}
		w2.Flush()
		return nil
	},
}

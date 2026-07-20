// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"fmt"

	"github.com/urfave/cli/v2"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
)

var inspecMetaData = cli.Command{
	Name:      "inspect",
	Aliases:   []string{"i", "info"},
	Usage:     "stat metadata of cubebox.",
	ArgsUsage: "CUBEBOX-ID [CUBEBOX-ID ...]",
	Action: func(context *cli.Context) error {
		var ids []string
		if context.Args().Len() > 0 {
			ids = context.Args().Slice()
		}
		if len(ids) == 0 {
			return fmt.Errorf("cubebox id is required")
		}

		conn, ctx, cancel, err := commands.NewGrpcConn(context)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()
		client := cubebox.NewCubeboxMgrClient(conn)

		var boxIDs []string
		req := &cubebox.ListCubeSandboxRequest{}
		resp, err := client.List(ctx, req)
		if err != nil {
			return err
		}
		for _, id := range ids {
			resolved, err := resolveSandboxIDFromList(resp.Items, id)
			if err != nil {
				return err
			}
			boxIDs = append(boxIDs, resolved)
		}

		for _, id := range boxIDs {
			req := &cubebox.ListCubeSandboxRequest{
				Id: &id,
				Option: &cubebox.ListCubeSandboxOption{
					PrivateWithCubeboxStore: true,
				},
			}
			resp, err := client.List(ctx, req)
			if err != nil {
				return err
			}
			for _, item := range resp.Items {
				if len(item.GetPrivateCubeboxStorageData()) == 0 {
					continue
				}
				fmt.Println(string(item.GetPrivateCubeboxStorageData()))
			}
		}
		return nil
	},
}

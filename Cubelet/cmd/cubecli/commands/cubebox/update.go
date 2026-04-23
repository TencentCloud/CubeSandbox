// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"encoding/json"
	"log"

	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
	"github.com/urfave/cli/v2"
)

var update = &cli.Command{
	Name:  "update",
	Flags: []cli.Flag{},
	Action: func(context *cli.Context) error {
		if context.NArg() == 0 {
			return errors.Wrap(errdefs.ErrInvalidArgument, "must specify at least one config file")
		}
		reqByte, err := readAllFile(context.Args().Slice()[0])
		if err != nil {
			log.Printf("readAllFile err. %s", err.Error())
		}
		req := &cubebox.UpdateCubeSandboxRequest{}
		if err := json.Unmarshal(reqByte, &req); err != nil {
			return err
		}
		req.RequestID = uuid.New().String()
		conn, ctx, cancel, err := commands.NewGrpcConn(context)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()
		client := cubebox.NewCubeboxMgrClient(conn)

		resp, err := client.Update(ctx, req)
		if err != nil {
			log.Printf("Update err. %s", err.Error())
			return err
		}
		log.Printf("Update %+v", utils.InterfaceToString(resp))
		return nil
	},
}

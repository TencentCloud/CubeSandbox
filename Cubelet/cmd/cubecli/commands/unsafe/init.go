// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package unsafe

import (
	"fmt"
	"log"

	"github.com/google/uuid"
	"github.com/urfave/cli/v2"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/nbi/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/ret"
)

var Init = &cli.Command{
	Name:  "init",
	Usage: "init cubelet",
	Flags: []cli.Flag{},
	Action: func(context *cli.Context) error {
		if !commands.AskForConfirm("init will destroy ALL of the resource, continue only if you confirm", 3) {
			return nil
		}

		conn, ctx, cancel, err := commands.NewGrpcConn(context)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()
		client := nbi.NewCubeLetClient(conn)
		req := &nbi.InitRequest{
			RequestID: uuid.New().String(),
		}
		rsp, err := client.InitHost(ctx, req)
		if err != nil {
			return err
		}
		if !ret.IsSuccessCode(rsp.GetCode()) {
			log.Printf("InitHost failure:%v, %v", req.RequestID, rsp.GetCode())
			return fmt.Errorf("failed to init host, code: %v, msg: %v", rsp.GetCode().String(), rsp.GetMessage())
		}
		log.Printf("InitHost rsp:%+v", rsp)
		return nil
	},
}

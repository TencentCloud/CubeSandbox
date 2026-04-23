// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package volume

import (
	"errors"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/urfave/cli/v2"
)

var resetvolumeref = &cli.Command{
	Name:  "resetvolumeref",
	Usage: "resetvolumeref volume",
	Flags: []cli.Flag{},
	Action: func(context *cli.Context) error {

		req := images.VolumUtilsRequest{
			Cmd: "resetVolumeRef",
		}
		conn, ctx, cancel, err := commands.NewGrpcConn(context)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()

		client := images.NewImagesClient(conn)
		req.RequestID = uuid.New().String()
		startTime := time.Now()
		resp, err := client.VolumeUtils(ctx, &req)
		cost := time.Since(startTime).Milliseconds()
		if err != nil {
			log.Printf("VolumeUtils err. %s. RequestId: %s", err.Error(), req.RequestID)
			time.Sleep(5 * time.Second)
			return err
		}
		log.Printf("VolumeUtils RequestId:%s,code:%d, message:%s,cost:%v", resp.RequestID, resp.Ret.RetCode, resp.Ret.RetMsg, cost)
		if resp.Ret.RetCode != errorcode.ErrorCode_Success {
			return errors.New(resp.Ret.RetMsg)
		}
		return nil
	},
}

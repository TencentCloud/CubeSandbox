// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/urfave/cli/v2"
)

var Create = &cli.Command{
	Name:      "create",
	Aliases:   []string{"test"},
	Usage:     "create cubebox from request file encorded in json",
	UsageText: "cubecli cubebox create [req.json]",
	Flags: []cli.Flag{
		&cli.DurationFlag{
			Name:        "sleep,s",
			Usage:       "sleep time before delete",
			DefaultText: "1s",
		},
		&cli.BoolFlag{
			Name:        "rm",
			Usage:       "rm pods while exit",
			DefaultText: "true",
		},
	},
	Action: func(context *cli.Context) error {
		conn, ctx, cancel, err := commands.NewGrpcConn(context)
		if err != nil {
			return err
		}
		defer conn.Close()
		defer cancel()

		for _, arg := range context.Args().Slice() {
			client := cubebox.NewCubeboxMgrClient(conn)
			myPrint("start create sandbox request file:%v", arg)
			req, err := readRunSandboxReqFromFile(arg)
			if err != nil {
				myPrint("failed to read req file: %v", err)
				os.Exit(1)
			}
			req.RequestID = uuid.New().String()

			reqb, _ := json.Marshal(req)
			myPrint("send create sandbox request: %s", string(reqb))
			rsp, err := client.Create(ctx, req)
			if err != nil {
				myPrint("create failure:%v", err)
				os.Exit(1)
			}

			respStr, err := json.Marshal(rsp)
			if err != nil {
				myPrint("failed to marshal resp: %v", err)
				os.Exit(1)
			}
			myPrint("create sandbox rspesponse: %s", respStr)
			if rsp.Ret.RetCode == errorcode.ErrorCode_Success {
				myPrint("create sandbox %s success", rsp.SandboxID)
				if context.Bool("rm") {
					duration := context.Duration("sleep")
					myPrint("sleep %v before destroy sandbox %s", duration, rsp.SandboxID)
					time.Sleep(duration)
					req := &cubebox.DestroyCubeSandboxRequest{
						RequestID: uuid.New().String(),
						SandboxID: rsp.SandboxID,
					}
					myPrint("start destroy sandbox %s", rsp.SandboxID)
					rsp, err := client.Destroy(ctx, req)
					if err != nil {
						myPrint("destroy sandbox failure:%v", err)
						return err
					}
					myPrint("destroy sandbox rsp:%v\n", rsp)
				} else {
					myPrint("skip to destroy sandbox, use -rm to remove sandbox")
				}
			} else {
				myPrint("create sandbox failure:%v: %v", rsp.Ret.RetCode, rsp.Ret.RetMsg)
				os.Exit(127)
			}
		}
		return nil
	},
}

func myPrint(format string, a ...interface{}) {
	fmt.Printf("%v,"+format+"\n",
		append([]interface{}{fmt.Sprintf("%v", time.Now().Format(time.RFC3339Nano))}, a...)...)
}

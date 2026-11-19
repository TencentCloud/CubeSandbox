// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	jsoniter "github.com/json-iterator/go"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/cmd/cubemastercli/commands"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

// ReleaseResumePinCommand releases an xfs-s3 Resume attempt pinned to a node
// that is down for good, so the sandbox can be resumed elsewhere.
var ReleaseResumePinCommand = cli.Command{
	Name:      "release-resume-pin",
	Usage:     "let a paused xfs-s3 sandbox resume on another node once the node of its last resume is confirmed down",
	ArgsUsage: "<sandbox-id>",
	Description: `A Resume of an xfs-s3 sandbox whose outcome is unknown pins the sandbox to
   that node, because the sandbox may be running there. While the node answers,
   resuming again settles it there and this command refuses. Run it only when
   the node is down and cannot run the sandbox any more (powered off, or its
   Cubelet and VMs stopped): otherwise the sandbox may run on two nodes.`,
	Flags: []cli.Flag{
		cli.BoolFlag{Name: "yes", Usage: "do not ask for confirmation"},
	},
	Action: func(c *cli.Context) error {
		if c.NArg() != 1 {
			_ = cli.ShowCommandHelp(c, "release-resume-pin")
			return errors.New("exactly one sandbox id is required")
		}
		sandboxID := c.Args().First()
		if !c.Bool("yes") && !commands.AskForConfirm(fmt.Sprintf(
			"Release the resume pin of %s? Only do this if its pinned node is down and cannot run the sandbox", sandboxID), 3) {
			return errors.New("not confirmed")
		}
		requestID := uuid.NewString()
		body, err := jsoniter.Marshal(&types.ReleaseResumePinRequest{RequestID: requestID, SandboxID: sandboxID})
		if err != nil {
			return err
		}
		rsp := &types.Res{}
		if err := doSnapshotReq(c, http.MethodPost, "/cube/sandbox/resume-pin/release", requestID, bytes.NewBuffer(body), rsp); err != nil {
			return err
		}
		if err := ensureSuccessRet(rsp.Ret); err != nil {
			return err
		}
		fmt.Println(rsp.Ret.RetMsg)
		return nil
	},
}

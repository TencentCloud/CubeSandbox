// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	jsoniter "github.com/json-iterator/go"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/cmd/cubemastercli/commands"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

var SnapshotRecoverCreateCommand = cli.Command{
	Name:  "recover-create",
	Usage: "release a stale create reservation after its node has been externally fenced",
	Flags: []cli.Flag{
		cli.StringFlag{Name: "snapshot-id", Usage: "retired snapshot id"},
		cli.StringFlag{Name: "sandbox-id", Usage: "reserved sandbox id"},
		cli.StringFlag{Name: "node-id", Usage: "exact node id stored in the reservation"},
		cli.StringFlag{Name: "node-ip", Usage: "exact node IP stored in the reservation"},
		cli.BoolFlag{Name: "confirm-node-fenced", Usage: "confirm the old node cannot run VMs or resume queued create RPCs"},
		cli.StringFlag{Name: "reason", Usage: "audit reason describing how the node was fenced"},
	},
	Action: func(c *cli.Context) error {
		snapshotID, sandboxID := strings.TrimSpace(c.String("snapshot-id")), strings.TrimSpace(c.String("sandbox-id"))
		nodeID, nodeIP := strings.TrimSpace(c.String("node-id")), strings.TrimSpace(c.String("node-ip"))
		reason := strings.TrimSpace(c.String("reason"))
		if snapshotID == "" || sandboxID == "" || (nodeID == "" && nodeIP == "") || reason == "" || !c.Bool("confirm-node-fenced") {
			return errors.New("snapshot-id, sandbox-id, node identity, reason and confirm-node-fenced are required")
		}
		body, err := jsoniter.Marshal(map[string]any{
			"sandbox_id": sandboxID, "node_id": nodeID, "node_ip": nodeIP,
			"confirm_node_fenced": true, "reason": reason,
		})
		if err != nil {
			return err
		}
		rsp := &types.Res{}
		if err := doSnapshotReq(c, http.MethodPost, "/cube/snapshot/"+url.PathEscape(snapshotID)+"/recover-create", uuid.NewString(), bytes.NewBuffer(body), rsp); err != nil {
			return err
		}
		if err := ensureSuccessRet(rsp.Ret); err != nil {
			return err
		}
		commands.PrintAsJSON(rsp)
		return nil
	},
}

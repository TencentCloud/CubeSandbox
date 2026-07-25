// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	commands "github.com/tencentcloud/CubeSandbox/CubeMaster/cmd/cubemastercli/commands"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/nodemeta"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

type nodeResponse struct {
	Ret  *types.Ret   `json:"ret,omitempty"`
	Data []*node.Node `json:"data,omitempty"`
}

type nodeMetaResponse struct {
	Ret  *types.Ret             `json:"ret,omitempty"`
	Data *nodemeta.NodeSnapshot `json:"data,omitempty"`
}

var nodeIsolationFlags = []cli.Flag{
	cli.BoolFlag{
		Name:  "json",
		Usage: "print raw json response",
	},
}

var NodeCommand = cli.Command{
	Name:    "node",
	Aliases: []string{"nodes"},
	Usage:   "list / isolate / unisolate cubemaster nodes",
	Subcommands: cli.Commands{
		NodeListCommand,
		NodeIsolateCommand,
		NodeUnisolateCommand,
	},
}

var NodeListCommand = cli.Command{
	Name:    "list",
	Aliases: []string{"ls"},
	Usage:   "list node status from cubemaster internal endpoint",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "hostid",
			Usage: "query single host/node id",
		},
		cli.BoolFlag{
			Name:  "score-only",
			Usage: "only query score/update timestamps",
		},
		cli.BoolFlag{
			Name:  "json",
			Usage: "print raw json response",
		},
	},
	Action: func(c *cli.Context) error {
		serverList = getServerAddrs(c)
		if len(serverList) == 0 {
			log.Printf("no server addr\n")
			return errors.New("no server addr")
		}
		port = c.GlobalString("port")
		requestID := uuid.New().String()
		host := serverList[rand.Int()%len(serverList)]

		url := fmt.Sprintf("http://%s/internal/node?requestID=%s", net.JoinHostPort(host, port), requestID)
		if hostID := c.String("hostid"); hostID != "" {
			url += "&host_id=" + hostID
		}
		if c.Bool("score-only") {
			url += "&score_only=true"
		}

		rsp := &nodeResponse{}
		if err := doHttpReq(c, url, http.MethodGet, requestID, nil, rsp); err != nil {
			log.Printf("node list request err. %s. RequestId: %s\n", err.Error(), requestID)
			return err
		}
		if rsp.Ret == nil {
			return errors.New("empty response")
		}
		if rsp.Ret.RetCode != 200 {
			log.Printf("node list failed. %s. RequestId: %s\n", rsp.Ret.RetMsg, requestID)
			return errors.New(rsp.Ret.RetMsg)
		}
		sort.Slice(rsp.Data, func(i, j int) bool {
			return rsp.Data[i].ID() < rsp.Data[j].ID()
		})
		if c.Bool("json") {
			commands.PrintAsJSON(rsp)
			return nil
		}
		printNodeSummary(rsp.Data, c.Bool("score-only"))
		return nil
	},
}

var NodeIsolateCommand = cli.Command{
	Name:      "isolate",
	Usage:     "cordon node(s) (block new sandbox scheduling; existing sandboxes unaffected)",
	ArgsUsage: "<node-id> [node-id ...]",
	Flags:     nodeIsolationFlags,
	Action: func(c *cli.Context) error {
		return doNodeIsolation(c, http.MethodPut)
	},
}

var NodeUnisolateCommand = cli.Command{
	Name:      "unisolate",
	Usage:     "remove cordon so the node(s) can receive new sandboxes",
	ArgsUsage: "<node-id> [node-id ...]",
	Flags:     nodeIsolationFlags,
	Action: func(c *cli.Context) error {
		return doNodeIsolation(c, http.MethodDelete)
	},
}

func doNodeIsolation(c *cli.Context, method string) error {
	if c.NArg() == 0 {
		cmd := "unisolate"
		if method == http.MethodPut {
			cmd = "isolate"
		}
		_ = cli.ShowCommandHelp(c, cmd)
		return errors.New("node id is required")
	}
	serverList = getServerAddrs(c)
	if len(serverList) == 0 {
		return errors.New("no server addr")
	}
	port = c.GlobalString("port")

	var opErr error
	for _, nodeID := range c.Args() {
		if err := isolateOneNode(c, method, nodeID); err != nil {
			log.Printf("%s failed: %s %s\n", isolationAction(method), nodeID, err.Error())
			opErr = errors.Join(opErr, fmt.Errorf("%s: %w", nodeID, err))
			continue
		}
	}
	return opErr
}

func isolationAction(method string) string {
	if method == http.MethodPut {
		return "isolated"
	}
	return "unisolated"
}

func isolateOneNode(c *cli.Context, method, nodeID string) error {
	requestID := uuid.New().String()
	host := serverList[rand.Int()%len(serverList)]
	u := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, port),
		Path:   "/internal/meta/nodes/" + url.PathEscape(nodeID) + "/isolation",
	}
	rsp := &nodeMetaResponse{}
	if err := doHttpReq(c, u.String(), method, requestID, bytes.NewReader(nil), rsp); err != nil {
		return err
	}
	if rsp.Ret == nil {
		return errors.New("empty response")
	}
	if rsp.Ret.RetCode != 200 {
		return errors.New(rsp.Ret.RetMsg)
	}
	if c.Bool("json") {
		commands.PrintAsJSON(rsp)
		return nil
	}
	disabled := false
	if rsp.Data != nil {
		disabled = rsp.Data.SchedulingDisabled
		nodeID = rsp.Data.NodeID
	}
	fmt.Printf("node %s %s: scheduling_disabled=%t\n", nodeID, isolationAction(method), disabled)
	return nil
}

func printNodeSummary(nodes []*node.Node, scoreOnly bool) {
	w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
	if scoreOnly {
		fmt.Fprintln(w, "NODE_ID\tSCORE\tMETRIC_UPDATE\tMETRIC_LOCAL_UPDATE\tMETADATA_UPDATE")
		for _, item := range nodes {
			fmt.Fprintf(w, "%s\t%.4f\t%s\t%s\t%s\n",
				item.ID(), item.Score,
				formatNodeTime(item.MetricUpdate),
				formatNodeTime(item.MetricLocalUpdateAt),
				formatNodeTime(item.MetaDataUpdateAt))
		}
		_ = w.Flush()
		return
	}
	fmt.Fprintln(w, "NODE_ID\tNODE_IP\tINSTANCE_TYPE\tZONE\tCPU_TYPE\tHEALTHY\tSCHEDULING_DISABLED\tHOST_STATUS")
	for _, item := range nodes {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%t\t%t\t%s\n",
			item.ID(), item.HostIP(), item.InstanceType, item.Zone, item.CPUType, item.Healthy,
			item.SchedulingDisabled(), item.HostStatus,
		)
	}
	_ = w.Flush()
}

func formatNodeTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04:05")
}

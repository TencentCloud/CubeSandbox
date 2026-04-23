// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	commands "github.com/tencentcloud/CubeSandbox/CubeMaster/cmd/cubemastercli/commands"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

type nodeResponse struct {
	Ret  *types.Ret   `json:"ret,omitempty"`
	Data []*node.Node `json:"data,omitempty"`
}

var NodeCommand = cli.Command{
	Name:    "node",
	Aliases: []string{"nodes"},
	Usage:   "list cubemaster nodes and status",
	Subcommands: cli.Commands{
		NodeListCommand,
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
	fmt.Fprintln(w, "NODE_ID\tNODE_IP\tINSTANCE_TYPE\tZONE\tCPU_TYPE\tHEALTHY\tHOST_STATUS")
	for _, item := range nodes {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%t\t%s\n",
			item.ID(), item.HostIP(), item.InstanceType, item.Zone, item.CPUType, item.Healthy, item.HostStatus,
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

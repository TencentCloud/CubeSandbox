// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/urfave/cli"
)

// CubeOps internal endpoints return the domain object directly, not an envelope.

var NodeCommand = cli.Command{
	Name:    "node",
	Aliases: []string{"nodes"},
	Usage:   "list / isolate / unisolate / delete CubeOps nodes",
	Subcommands: []cli.Command{
		{
			Name:    "list",
			Aliases: []string{"ls"},
			Usage:   "list node status from CubeOps internal endpoint",
			Flags: []cli.Flag{
				cli.StringFlag{Name: "hostid", Usage: "query single host/node id"},
				cli.BoolFlag{Name: "score-only", Usage: "only query score/update timestamps"},
				cli.BoolFlag{Name: "show-local-templates", Usage: "show local templates in table/json output"},
				cli.BoolFlag{Name: "json", Usage: "print raw json response"},
			},
			Action: listAction,
		},
		{
			Name:      "isolate",
			Usage:     "cordon node(s)",
			ArgsUsage: "<node-id> [node-id ...]",
			Flags:     []cli.Flag{cli.BoolFlag{Name: "json", Usage: "print raw json response"}},
			Action: func(c *cli.Context) error {
				return doIsolation(c, http.MethodPut)
			},
		},
		{
			Name:      "unisolate",
			Usage:     "remove cordon from node(s)",
			ArgsUsage: "<node-id> [node-id ...]",
			Flags:     []cli.Flag{cli.BoolFlag{Name: "json", Usage: "print raw json response"}},
			Action: func(c *cli.Context) error {
				return doIsolation(c, http.MethodDelete)
			},
		},
		{
			Name:      "delete",
			Aliases:   []string{"rm"},
			Usage:     "delete isolated, empty node(s)",
			ArgsUsage: "<node-id> [node-id ...]",
			Flags: []cli.Flag{
				cli.BoolFlag{Name: "force", Usage: "delete without verifying sandbox inventory"},
				cli.BoolFlag{Name: "json", Usage: "print raw json response"},
			},
			Action: deleteAction,
		},
	},
}

func listAction(c *cli.Context) error {
	serverList = getServerAddrs(c)
	if len(serverList) == 0 {
		return errors.New("no server addr")
	}
	port = c.GlobalString("port")
	requestID := uuid.New().String()
	host, err := pickHost()
	if err != nil {
		return err
	}

	hostID := c.String("hostid")
	scoreOnly := c.Bool("score-only")
	var rsp []*model.SchedulerNode
	if hostID != "" {
		reqURL := buildURL(host, fmt.Sprintf("/internal/v1/nodes/%s?requestID=%s", url.PathEscape(hostID), requestID))
		if scoreOnly {
			reqURL += "&score_only=true"
		}
		var single *model.SchedulerNode
		if err := doHttpReq(c, reqURL, http.MethodGet, requestID, nil, &single); err != nil {
			return err
		}
		if single != nil {
			rsp = []*model.SchedulerNode{single}
		}
	} else {
		reqURL := buildURL(host, fmt.Sprintf("/internal/v1/nodes?requestID=%s", requestID))
		if scoreOnly {
			reqURL += "&score_only=true"
		}
		if err := doHttpReq(c, reqURL, http.MethodGet, requestID, nil, &rsp); err != nil {
			return err
		}
		sort.Slice(rsp, func(i, j int) bool { return rsp[i].ID() < rsp[j].ID() })
	}
	if c.Bool("json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if c.Bool("show-local-templates") {
			return enc.Encode(rsp)
		}
		return enc.Encode(stripLocalTemplates(rsp))
	}
	printNodeSummary(rsp, c.Bool("score-only"), c.Bool("show-local-templates"))
	return nil
}

// stripLocalTemplates returns a shallow copy of nodes with LocalTemplates
// cleared so that --json default output does not include the (often large)
// stale snapshot list. The original nodes are not modified.
func stripLocalTemplates(nodes []*model.SchedulerNode) []*model.SchedulerNode {
	out := make([]*model.SchedulerNode, len(nodes))
	for i, n := range nodes {
		if n == nil {
			continue
		}
		cp := *n
		cp.LocalTemplates = nil
		out[i] = &cp
	}
	return out
}

func doIsolation(c *cli.Context, method string) error {
	if c.NArg() == 0 {
		return errors.New("node id is required")
	}
	serverList = getServerAddrs(c)
	if len(serverList) == 0 {
		return errors.New("no server addr")
	}
	port = c.GlobalString("port")

	var opErr error
	for _, nodeID := range c.Args() {
		if err := isolateOne(c, method, nodeID); err != nil {
			opErr = errors.Join(opErr, fmt.Errorf("%s: %w", nodeID, err))
		}
	}
	return opErr
}

func isolateOne(c *cli.Context, method, nodeID string) error {
	requestID := uuid.New().String()
	host, err := pickHost()
	if err != nil {
		return err
	}
	u := buildURL(host, fmt.Sprintf("/internal/v1/nodes/%s/isolation?requestID=%s", url.PathEscape(nodeID), requestID))
	var rsp *model.NodeSnapshot
	if err := doHttpReq(c, u, method, requestID, marshalBody(nil), &rsp); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rsp)
	}
	disabled := false
	if rsp != nil {
		disabled = rsp.SchedulingDisabled
		nodeID = rsp.NodeID
	}
	fmt.Printf("node %s %s: scheduling_disabled=%t\n", nodeID, isolationAction(method), disabled)
	return nil
}

func isolationAction(method string) string {
	if method == http.MethodPut {
		return "isolated"
	}
	return "unisolated"
}

func printNodeSummary(nodes []*model.SchedulerNode, scoreOnly, showLocalTemplates bool) {
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
	if showLocalTemplates {
		fmt.Fprintln(w, "NODE_ID\tNODE_IP\tINSTANCE_TYPE\tZONE\tCPU_TYPE\tHEALTHY\tSCHEDULING_DISABLED\tHOST_STATUS\tLOCAL_TEMPLATES")
		for _, item := range nodes {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%t\t%t\t%s\t%d\n",
				item.ID(), item.HostIP(), item.InstanceType, item.Zone, item.CPUType, item.Healthy,
				item.SchedulingDisabled, item.HostStatus, len(item.LocalTemplates))
		}
	} else {
		fmt.Fprintln(w, "NODE_ID\tNODE_IP\tINSTANCE_TYPE\tZONE\tCPU_TYPE\tHEALTHY\tSCHEDULING_DISABLED\tHOST_STATUS")
		for _, item := range nodes {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%t\t%t\t%s\n",
				item.ID(), item.HostIP(), item.InstanceType, item.Zone, item.CPUType, item.Healthy,
				item.SchedulingDisabled, item.HostStatus)
		}
	}
	_ = w.Flush()
}

func formatNodeTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04:05")
}

// deleteAction removes one or more nodes; a failure on one does not abort
// the rest, and the command exits non-zero if any deletion failed.
func deleteAction(c *cli.Context) error {
	if c.NArg() == 0 {
		_ = cli.ShowCommandHelp(c, "delete")
		return errors.New("node id is required")
	}
	serverList = getServerAddrs(c)
	if len(serverList) == 0 {
		return errors.New("no server addr")
	}
	port = c.GlobalString("port")

	var opErr error
	for _, nodeID := range c.Args() {
		if err := deleteOne(c, nodeID); err != nil {
			fmt.Fprintf(os.Stderr, "delete failed: %s %s\n", nodeID, err.Error())
			opErr = errors.Join(opErr, fmt.Errorf("%s: %w", nodeID, err))
		}
	}
	return opErr
}

// deleteOne issues DELETE /internal/v1/nodes/{nodeID}?force=... and prints
// the per-node result.
func deleteOne(c *cli.Context, nodeID string) error {
	requestID := uuid.New().String()
	host, err := pickHost()
	if err != nil {
		return err
	}
	force := c.Bool("force")
	reqURL := buildURL(host, fmt.Sprintf("/internal/v1/nodes/%s?requestID=%s", url.PathEscape(nodeID), requestID))
	if force {
		reqURL += "&force=true"
	}
	var rsp *model.NodeSnapshot
	if err := doHttpReq(c, reqURL, http.MethodDelete, requestID, marshalBody(nil), &rsp); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rsp)
	}
	fmt.Printf("node %s deleted\n", nodeID)
	return nil
}

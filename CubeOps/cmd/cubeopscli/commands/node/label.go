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
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/urfave/cli"
)

// labelCommand manages node labels through the CubeOps internal API.
//
//	set     = merge; keys not passed are preserved
//	replace = rebuild the user label set; unlisted user labels are removed
//	delete  = remove one user key
//
// Isolation and machine-type are not writable here.

var labelCommand = cli.Command{
	Name:  "label",
	Usage: "list / set / replace / delete node labels",
	Subcommands: []cli.Command{
		{
			Name:      "list",
			Usage:     "show all labels of one or more nodes",
			ArgsUsage: "<node-id> [node-id...]",
			Flags:     []cli.Flag{cli.BoolFlag{Name: "json", Usage: "print raw json response"}},
			Action:    labelListAction,
		},
		{
			Name:      "set",
			Usage:     "merge labels into one or more nodes (other keys preserved)",
			ArgsUsage: "<node-id> [node-id...] key=value [key=value...]",
			Description: "Node ids come first and stop at the first key=value argument. " +
				"Each node is updated independently; one failure does not stop the rest.",
			Flags: []cli.Flag{
				cli.StringFlag{Name: "operator", Value: "cli", Usage: "operator name recorded in the audit"},
				cli.BoolFlag{Name: "json", Usage: "print structured per-node results"},
			},
			Action: func(c *cli.Context) error { return labelWriteAction(c, http.MethodPatch) },
		},
		{
			Name:      "replace",
			Usage:     "replace the complete user label set of one or more nodes",
			ArgsUsage: "<node-id> [node-id...] key=value [key=value...]",
			Description: "WARNING: user keys not listed are deleted. Isolation and machine-type " +
				"are preserved. Change machine-type with `cubeopscli machine-type`.",
			Flags: []cli.Flag{
				cli.StringFlag{Name: "operator", Value: "cli", Usage: "operator name recorded in the audit"},
				cli.BoolFlag{Name: "json", Usage: "print structured per-node results"},
			},
			Action: func(c *cli.Context) error { return labelWriteAction(c, http.MethodPut) },
		},
		{
			Name:      "delete",
			Usage:     "delete one label key from one or more nodes",
			ArgsUsage: "<node-id> [node-id...] <key>",
			Flags: []cli.Flag{
				cli.StringFlag{Name: "operator", Value: "cli", Usage: "operator name recorded in the audit"},
				cli.BoolFlag{Name: "json", Usage: "print structured per-node results"},
			},
			Action: labelDeleteAction,
		},
	},
}

// labelResult is the per-node outcome of a batch write.
type labelResult struct {
	NodeID string `json:"node_id"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

func labelListAction(c *cli.Context) error {
	if c.NArg() == 0 {
		_ = cli.ShowCommandHelp(c, c.Command.Name)
		return errors.New("at least one node id is required")
	}
	if err := initNodeHTTP(c); err != nil {
		return err
	}
	type nodeLabels struct {
		NodeID string            `json:"node_id"`
		Labels map[string]string `json:"labels"`
	}
	out := make([]nodeLabels, 0, c.NArg())
	for _, nodeID := range c.Args() {
		snap, err := getNode(c, nodeID)
		if err != nil {
			return fmt.Errorf("%s: %w", nodeID, err)
		}
		out = append(out, nodeLabels{NodeID: snap.ID(), Labels: snap.NodeLabels})
	}
	if c.Bool("json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
	fmt.Fprintln(w, "NODE_ID\tKEY\tVALUE")
	for _, item := range out {
		keys := make([]string, 0, len(item.Labels))
		for k := range item.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "%s\t%s\t%s\n", item.NodeID, k, item.Labels[k])
		}
	}
	return w.Flush()
}

func labelWriteAction(c *cli.Context, method string) error {
	nodeIDs, labels, err := splitNodeIDsAndKV(c.Args())
	if err != nil {
		return err
	}
	if len(nodeIDs) == 0 {
		return errors.New("at least one node id is required")
	}
	if len(labels) == 0 && method != http.MethodPut {
		return errors.New("at least one key=value label is required")
	}
	if err := rejectMachineTypeLabel(labels, ""); err != nil {
		return err
	}
	if err := initNodeHTTP(c); err != nil {
		return err
	}
	// Replace with no key=value is an explicit "clear user labels" request;
	// send {} so the server can tell it apart from a malformed request.
	if labels == nil {
		labels = map[string]string{}
	}
	body := model.UpdateNodeLabelsRequest{Labels: labels}
	results := make([]labelResult, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		id := uuid.New().String()
		host, err := pickHost()
		if err != nil {
			return err
		}
		path := fmt.Sprintf("/internal/v1/nodes/%s/labels", url.PathEscape(nodeID))
		if method == http.MethodPut {
			// PUT is the historical merge route; replace has its own path.
			path += "/replace"
		}
		u := buildURL(host, fmt.Sprintf("%s?requestID=%s&operator=%s",
			path, id, url.QueryEscape(c.String("operator"))))
		if err := doHttpReq(c, u, method, id, marshalBody(body), nil); err != nil {
			results = append(results, labelResult{NodeID: nodeID, Error: err.Error()})
			continue
		}
		results = append(results, labelResult{NodeID: nodeID, OK: true})
	}
	return printLabelResults(c, method, results)
}

func labelDeleteAction(c *cli.Context) error {
	args := c.Args()
	if len(args) < 2 {
		_ = cli.ShowCommandHelp(c, c.Command.Name)
		return errors.New("at least one node id and one label key are required")
	}
	nodeIDs := args[:len(args)-1]
	key := args[len(args)-1]
	if strings.Contains(key, "=") {
		return errors.New("last argument must be the label key, not key=value")
	}
	if err := rejectMachineTypeLabel(nil, key); err != nil {
		return err
	}
	if err := initNodeHTTP(c); err != nil {
		return err
	}
	results := make([]labelResult, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		id := uuid.New().String()
		host, err := pickHost()
		if err != nil {
			return err
		}
		// The query form carries keys containing '/' safely.
		u := buildURL(host, fmt.Sprintf("/internal/v1/nodes/%s/labels?key=%s&requestID=%s&operator=%s",
			url.PathEscape(nodeID), url.QueryEscape(key), id, url.QueryEscape(c.String("operator"))))
		if err := doHttpReq(c, u, http.MethodDelete, id, nil, nil); err != nil {
			results = append(results, labelResult{NodeID: nodeID, Error: err.Error()})
			continue
		}
		results = append(results, labelResult{NodeID: nodeID, OK: true})
	}
	return printLabelResults(c, http.MethodDelete, results)
}

func printLabelResults(c *cli.Context, op string, results []labelResult) error {
	if c.Bool("json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]interface{}{"operation": op, "results": results}); err != nil {
			return err
		}
	} else {
		w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
		fmt.Fprintln(w, "NODE_ID\tRESULT\tERROR")
		for _, r := range results {
			status := "ok"
			if !r.OK {
				status = "failed"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", r.NodeID, status, r.Error)
		}
		_ = w.Flush()
	}
	failed := 0
	for _, r := range results {
		if !r.OK {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d node(s) failed", failed, len(results))
	}
	return nil
}

// splitNodeIDsAndKV treats leading non key=value arguments as node ids and the
// remaining arguments as labels.
func splitNodeIDsAndKV(args []string) ([]string, map[string]string, error) {
	var nodeIDs []string
	i := 0
	for ; i < len(args); i++ {
		if strings.Contains(args[i], "=") {
			break
		}
		nodeIDs = append(nodeIDs, args[i])
	}
	if i == len(args) {
		return nodeIDs, nil, nil
	}
	labels := make(map[string]string, len(args)-i)
	for _, item := range args[i:] {
		k, v, ok := strings.Cut(item, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, nil, fmt.Errorf("invalid label %q: expected key=value", item)
		}
		labels[k] = v
	}
	return nodeIDs, labels, nil
}

func initNodeHTTP(c *cli.Context) error {
	serverList = getServerAddrs(c)
	if len(serverList) == 0 {
		return errors.New("no server addr")
	}
	port = c.GlobalString("port")
	return nil
}

func getNode(c *cli.Context, nodeID string) (*model.SchedulerNode, error) {
	id := uuid.New().String()
	host, err := pickHost()
	if err != nil {
		return nil, err
	}
	u := buildURL(host, fmt.Sprintf("/internal/v1/nodes/%s?requestID=%s", url.PathEscape(nodeID), id))
	var snap model.SchedulerNode
	if err := doHttpReq(c, u, http.MethodGet, id, nil, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

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
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/urfave/cli"
)

// MachineTypeCommand manages the machine type catalog and node membership.
// It is mounted both at the top level and under `node`.
var MachineTypeCommand = cli.Command{
	Name:    "machine-type",
	Aliases: []string{"mt"},
	Usage:   "create and bind machine types used for cross-node restore",
	Subcommands: []cli.Command{
		{
			Name:   "list",
			Usage:  "list machine types",
			Flags:  []cli.Flag{cli.BoolFlag{Name: "json", Usage: "print json"}},
			Action: machineTypeListAction,
		},
		{
			Name:      "get",
			Usage:     "show one machine type and its nodes",
			ArgsUsage: "<name>",
			Flags:     []cli.Flag{cli.BoolFlag{Name: "json", Usage: "print json"}},
			Action:    machineTypeGetAction,
		},
		{
			Name:      "create",
			Usage:     "create a machine type and optionally bind nodes",
			ArgsUsage: "<name>",
			Flags: []cli.Flag{
				cli.StringFlag{Name: "display-name", Usage: "operator-facing name"},
				cli.StringFlag{Name: "description", Usage: "optional description"},
				cli.StringFlag{Name: "nodes", Usage: "comma-separated node ids to bind"},
				cli.StringFlag{Name: "operator", Value: "cli", Usage: "operator recorded in the audit"},
				cli.BoolFlag{Name: "json", Usage: "print json"},
			},
			Action: machineTypeCreateAction,
		},
		{
			Name:      "update",
			Usage:     "update the display name or description",
			ArgsUsage: "<name>",
			Flags: []cli.Flag{
				cli.StringFlag{Name: "display-name", Usage: "operator-facing name"},
				cli.StringFlag{Name: "description", Usage: "optional description"},
				cli.StringFlag{Name: "operator", Value: "cli", Usage: "operator recorded in the audit"},
				cli.BoolFlag{Name: "json", Usage: "print json"},
			},
			Action: machineTypeUpdateAction,
		},
		{
			Name:      "delete",
			Usage:     "delete a machine type",
			ArgsUsage: "<name>",
			Flags: []cli.Flag{
				cli.BoolFlag{Name: "unbind", Usage: "clear the machine type from bound nodes first"},
				cli.StringFlag{Name: "operator", Value: "cli", Usage: "operator recorded in the audit"},
				cli.BoolFlag{Name: "json", Usage: "print json"},
			},
			Action: machineTypeDeleteAction,
		},
		{
			Name:      "bind",
			Usage:     "bind nodes to a machine type, moving them off any previous type",
			ArgsUsage: "<name> <node-id> [node-id...]",
			Flags: []cli.Flag{
				cli.StringFlag{Name: "operator", Value: "cli", Usage: "operator recorded in the audit"},
				cli.BoolFlag{Name: "json", Usage: "print json"},
			},
			Action: func(c *cli.Context) error { return machineTypeMembersAction(c, http.MethodPost) },
		},
		{
			Name:      "unbind",
			Usage:     "remove nodes from their current machine type",
			ArgsUsage: "<node-id> [node-id...]",
			Flags: []cli.Flag{
				cli.StringFlag{Name: "operator", Value: "cli", Usage: "operator recorded in the audit"},
				cli.BoolFlag{Name: "json", Usage: "print json"},
			},
			Action: machineTypeUnbindAction,
		},
	},
}

func machineTypeListAction(c *cli.Context) error {
	if err := initNodeHTTP(c); err != nil {
		return err
	}
	var views []*model.MachineTypeView
	if err := machineTypeReq(c, http.MethodGet, "/internal/v1/machine-types", nil, &views); err != nil {
		return err
	}
	if c.Bool("json") {
		return printJSON(views)
	}
	w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
	fmt.Fprintln(w, "NAME\tDISPLAY\tNODES\tHEALTHY\tCPU_GROUPS\tKERNELS\tWARNINGS")
	for _, view := range views {
		if view == nil {
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%s\n",
			view.Name, displayOr(view.DisplayName, view.Name), view.NodeCount, view.HealthyNodeCount,
			len(view.CPUIDGroups), len(view.KernelReleases), strings.Join(view.Warnings, ","))
	}
	return w.Flush()
}

func machineTypeGetAction(c *cli.Context) error {
	if c.NArg() != 1 {
		return errors.New("machine type name is required")
	}
	if err := initNodeHTTP(c); err != nil {
		return err
	}
	var view model.MachineTypeView
	path := "/internal/v1/machine-types/" + url.PathEscape(c.Args().First())
	if err := machineTypeReq(c, http.MethodGet, path, nil, &view); err != nil {
		return err
	}
	if c.Bool("json") {
		return printJSON(view)
	}
	fmt.Printf("name: %s\ndisplay: %s\ndescription: %s\nnodes: %d healthy / %d total\nwarnings: %s\n",
		view.Name, displayOr(view.DisplayName, "-"), displayOr(view.Description, "-"),
		view.HealthyNodeCount, view.NodeCount, displayOr(strings.Join(view.Warnings, ","), "-"))
	w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
	fmt.Fprintln(w, "NODE_ID\tHOST_IP\tHEALTHY\tCPU_MODEL\tCPUID_HASH\tKERNEL")
	for _, n := range view.Nodes {
		fmt.Fprintf(w, "%s\t%s\t%t\t%s\t%s\t%s\n",
			n.NodeID, n.HostIP, n.Healthy, n.CPUModel, previewHash(n.CPUIDHash), n.HostKernelRelease)
	}
	return w.Flush()
}

func machineTypeCreateAction(c *cli.Context) error {
	if c.NArg() != 1 {
		return errors.New("machine type name is required")
	}
	if err := initNodeHTTP(c); err != nil {
		return err
	}
	body := model.CreateMachineTypeRequest{
		Name:        c.Args().First(),
		DisplayName: c.String("display-name"),
		Description: c.String("description"),
		NodeIDs:     splitCSV(c.String("nodes")),
	}
	var view model.MachineTypeView
	if err := machineTypeReq(c, http.MethodPost, "/internal/v1/machine-types", body, &view); err != nil {
		return err
	}
	if c.Bool("json") {
		return printJSON(view)
	}
	fmt.Printf("created machine type %s (%d node(s))\n", view.Name, view.NodeCount)
	return nil
}

func machineTypeUpdateAction(c *cli.Context) error {
	if c.NArg() != 1 {
		return errors.New("machine type name is required")
	}
	if !c.IsSet("display-name") && !c.IsSet("description") {
		return errors.New("set --display-name and/or --description")
	}
	if err := initNodeHTTP(c); err != nil {
		return err
	}
	body := map[string]string{}
	if c.IsSet("display-name") {
		body["displayName"] = c.String("display-name")
	}
	if c.IsSet("description") {
		body["description"] = c.String("description")
	}
	var view model.MachineTypeView
	path := "/internal/v1/machine-types/" + url.PathEscape(c.Args().First())
	if err := machineTypeReq(c, http.MethodPatch, path, body, &view); err != nil {
		return err
	}
	if c.Bool("json") {
		return printJSON(view)
	}
	fmt.Printf("updated machine type %s\n", view.Name)
	return nil
}

func machineTypeDeleteAction(c *cli.Context) error {
	if c.NArg() != 1 {
		return errors.New("machine type name is required")
	}
	if err := initNodeHTTP(c); err != nil {
		return err
	}
	path := "/internal/v1/machine-types/" + url.PathEscape(c.Args().First())
	if c.Bool("unbind") {
		path += "?unbind=true"
	}
	if err := machineTypeReq(c, http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	fmt.Printf("deleted machine type %s\n", c.Args().First())
	return nil
}

func machineTypeMembersAction(c *cli.Context, method string) error {
	if c.NArg() < 2 {
		return errors.New("machine type name and at least one node id are required")
	}
	if err := initNodeHTTP(c); err != nil {
		return err
	}
	name := c.Args().First()
	body := model.MachineTypeNodesRequest{NodeIDs: append([]string{}, c.Args().Tail()...)}
	path := "/internal/v1/machine-types/" + url.PathEscape(name) + "/nodes"
	var result model.MachineTypeBindResult
	if err := machineTypeReq(c, method, path, body, &result); err != nil {
		return err
	}
	return printMachineTypeResults(c, result.Results)
}

func machineTypeUnbindAction(c *cli.Context) error {
	if c.NArg() == 0 {
		return errors.New("at least one node id is required")
	}
	if err := initNodeHTTP(c); err != nil {
		return err
	}
	var failed error
	results := make([]model.MachineTypeNodeResult, 0, c.NArg())
	for _, nodeID := range c.Args() {
		path := fmt.Sprintf("/internal/v1/nodes/%s/machine-type", url.PathEscape(nodeID))
		if err := machineTypeReq(c, http.MethodDelete, path, nil, nil); err != nil {
			results = append(results, model.MachineTypeNodeResult{NodeID: nodeID, Error: err.Error()})
			failed = err
			continue
		}
		results = append(results, model.MachineTypeNodeResult{NodeID: nodeID, OK: true})
	}
	if err := printMachineTypeResults(c, results); err != nil {
		return err
	}
	if failed != nil && !c.Bool("json") {
		return errors.New("one or more nodes failed")
	}
	return nil
}

func printMachineTypeResults(c *cli.Context, results []model.MachineTypeNodeResult) error {
	if c.Bool("json") {
		return printJSON(map[string]interface{}{"results": results})
	}
	w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
	fmt.Fprintln(w, "NODE_ID\tRESULT\tERROR")
	failed := 0
	for _, r := range results {
		status := "ok"
		if !r.OK {
			status = "failed"
			failed++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.NodeID, status, r.Error)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d node(s) failed", failed, len(results))
	}
	return nil
}

func machineTypeReq(c *cli.Context, method, path string, body interface{}, rsp interface{}) error {
	id := uuid.New().String()
	host, err := pickHost()
	if err != nil {
		return err
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	u := buildURL(host, fmt.Sprintf("%s%srequestID=%s&operator=%s",
		path, sep, id, url.QueryEscape(c.String("operator"))))
	var reader = marshalBody(nil)
	if body != nil {
		reader = marshalBody(body)
	}
	return doHttpReq(c, u, method, id, reader, rsp)
}

func filterSchedulerNodes(nodes []*model.SchedulerNode, machineType string, unassigned bool) []*model.SchedulerNode {
	if machineType == "" && !unassigned {
		return nodes
	}
	filter := machineType
	if unassigned {
		filter = model.MachineTypeUnassigned
	}
	out := make([]*model.SchedulerNode, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if model.MachineTypeMatches(model.MachineTypeValue(n.NodeLabels), filter) {
			out = append(out, n)
		}
	}
	return out
}

func machineTypeColumn(n *model.SchedulerNode) string {
	if n == nil {
		return "-"
	}
	value := model.MachineTypeValue(n.NodeLabels)
	if value == "" {
		return "-"
	}
	return value
}

func rejectMachineTypeLabel(labels map[string]string, key string) error {
	if key == model.LabelMachineType {
		return errors.New("machine-type is managed by `cubeopscli machine-type bind/unbind`, not `node label`")
	}
	if _, ok := labels[model.LabelMachineType]; ok {
		return errors.New("machine-type is managed by `cubeopscli machine-type bind/unbind`, not `node label`")
	}
	return nil
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func displayOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func printJSON(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

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
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/urfave/cli"
)

// quotaCommand manages the desired node quota spec via the internal routes.
// The PUT is a full replace: fields left unset fall back to cubelet defaults.
var quotaCommand = cli.Command{
	Name:  "quota",
	Usage: "get / set / history node quota (desired spec vs heartbeat-reported actual)",
	Subcommands: []cli.Command{
		{
			Name:      "get",
			Usage:     "show spec vs actual vs drift for one node",
			ArgsUsage: "<node-id>",
			Flags:     []cli.Flag{cli.BoolFlag{Name: "json", Usage: "print raw json response"}},
			Action:    quotaGetAction,
		},
		{
			Name:      "set",
			Usage:     "replace the desired quota spec and push it to ops-agent",
			ArgsUsage: "<node-id>",
			Flags: []cli.Flag{
				cli.Int64Flag{Name: "mcpu", Usage: "cpu quota in milli-cores"},
				cli.StringFlag{Name: "mem", Usage: "mem quota quantity, e.g. 262144Mi / 256Gi"},
				cli.Int64Flag{Name: "mvm", Usage: "max mvm num"},
				cli.Int64Flag{Name: "create-concurrent", Usage: "creation concurrency"},
				cli.Float64Flag{Name: "paused-ratio", Usage: "explicit paused-release ratio in [0,1] (overrides the cluster default)"},
				cli.BoolFlag{Name: "paused-ratio-inherit", Usage: "clear the override: follow the cluster default (mutually exclusive with --paused-ratio)"},
				cli.Int64Flag{Name: "revision", Usage: "expected revision for compare-and-set (0 = use the revision read before this set; conflicts return 409)"},
				cli.StringFlag{Name: "operator", Value: "cli", Usage: "operator name recorded in the audit"},
				cli.BoolFlag{Name: "json", Usage: "print raw json response"},
			},
			Action: quotaSetAction,
		},
		{
			Name:      "history",
			Usage:     "list set-quota audit entries",
			ArgsUsage: "<node-id>",
			Flags: []cli.Flag{
				cli.IntFlag{Name: "limit", Value: 50, Usage: "max entries (1-500)"},
				cli.BoolFlag{Name: "json", Usage: "print raw json response"},
			},
			Action: quotaHistoryAction,
		},
	},
}

// quotaTargetNode resolves the target node id and wires the server list.
func quotaTargetNode(c *cli.Context) (string, error) {
	if c.NArg() == 0 {
		_ = cli.ShowCommandHelp(c, c.Command.Name)
		return "", errors.New("node id is required")
	}
	serverList = getServerAddrs(c)
	if len(serverList) == 0 {
		return "", errors.New("no server addr")
	}
	port = c.GlobalString("port")
	return c.Args().First(), nil
}

func quotaGetAction(c *cli.Context) error {
	nodeID, err := quotaTargetNode(c)
	if err != nil {
		return err
	}
	requestID := uuid.New().String()
	host, err := pickHost()
	if err != nil {
		return err
	}
	u := buildURL(host, fmt.Sprintf("/internal/v1/nodes/%s/config/quota?requestID=%s", url.PathEscape(nodeID), requestID))
	var view model.QuotaView
	if err := doHttpReq(c, u, http.MethodGet, requestID, nil, &view); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(&view)
	}
	printQuotaView(&view)
	return nil
}

// printQuotaView renders the effective quota and drift; spec values live in --json, history, and the drift note.
func printQuotaView(v *model.QuotaView) {
	actualRatio := "-"
	if v.Actual.PausedReleaseRatio != nil {
		actualRatio = fmt.Sprintf("%g", *v.Actual.PausedReleaseRatio)
	}
	w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
	fmt.Fprintf(w, "NODE_ID\t%s\n", v.NodeID)
	// mem and maxMvm are MiB-valued; maxMvm is the derived mvm ceiling.
	fmt.Fprintf(w, "ACTUAL\tmcpu=%d\tmem=%dMi\tmaxMvm=%d\tcreateConcurrent=%d\tpausedRatio=%s\n",
		v.Actual.MilliCPU, v.Actual.MemMB, v.Actual.MaxMvmNum, v.Actual.CreateConcurrentNum, actualRatio)
	fmt.Fprintf(w, "DRIFT\t%s\n", v.Drift)
	_ = w.Flush()
	if v.Message != "" {
		fmt.Printf("note: %s\n", v.Message)
	}
}

func quotaSetAction(c *cli.Context) error {
	nodeID, err := quotaTargetNode(c)
	if err != nil {
		return err
	}
	if !c.IsSet("mcpu") && !c.IsSet("mem") && !c.IsSet("mvm") && !c.IsSet("create-concurrent") &&
		!c.IsSet("paused-ratio") && !c.Bool("paused-ratio-inherit") {
		return errors.New("nothing to set: pass at least one of --mcpu/--mem/--mvm/--create-concurrent/--paused-ratio/--paused-ratio-inherit")
	}
	if c.Bool("paused-ratio-inherit") && c.IsSet("paused-ratio") {
		return errors.New("--paused-ratio-inherit and --paused-ratio are mutually exclusive")
	}
	requestID := uuid.New().String()
	host, err := pickHost()
	if err != nil {
		return err
	}

	// Incremental: overlay passed flags on the fetched spec; the fetched
	// revision doubles as CAS so a concurrent writer gets a 409.
	viewURL := buildURL(host, fmt.Sprintf("/internal/v1/nodes/%s/config/quota?requestID=%s", url.PathEscape(nodeID), requestID))
	var view model.QuotaView
	if err := doHttpReq(c, viewURL, http.MethodGet, requestID, nil, &view); err != nil {
		return err
	}
	spec := model.QuotaSpec{}
	if view.Spec != nil {
		spec = *view.Spec
		spec.ExpectedRevision = view.Spec.Revision
	}
	if c.IsSet("mcpu") {
		spec.MCpuLimit = c.Int64("mcpu")
	}
	if c.IsSet("mem") {
		spec.MemLimit = c.String("mem")
	}
	if c.IsSet("mvm") {
		spec.MvmLimit = c.Int64("mvm")
	}
	if c.IsSet("create-concurrent") {
		spec.CreationConcurrentNum = c.Int64("create-concurrent")
	}
	if c.Bool("paused-ratio-inherit") {
		spec.PausedReleaseRatio = nil // explicit null: follow the cluster default
	} else if c.IsSet("paused-ratio") {
		v := c.Float64("paused-ratio")
		spec.PausedReleaseRatio = &v
	}
	if c.IsSet("revision") {
		spec.ExpectedRevision = c.Int64("revision")
	}

	putID := uuid.New().String()
	q := url.Values{}
	q.Set("requestID", putID)
	q.Set("operator", c.String("operator"))
	u := buildURL(host, fmt.Sprintf("/internal/v1/nodes/%s/config/quota?%s", url.PathEscape(nodeID), q.Encode()))
	var out struct {
		View model.QuotaView  `json:"view"`
		Push model.PushResult `json:"push"`
	}
	if err := doHttpReq(c, u, http.MethodPut, putID, marshalBody(&spec), &out); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(&out)
	}
	if out.Push.Applied {
		fmt.Printf("node %s quota updated and applied\n", nodeID)
	} else {
		fmt.Printf("node %s quota updated; not applied yet: %s (retrying automatically)\n", nodeID, out.Push.SkipReason)
	}
	return nil
}

func quotaHistoryAction(c *cli.Context) error {
	nodeID, err := quotaTargetNode(c)
	if err != nil {
		return err
	}
	limit := c.Int("limit")
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	requestID := uuid.New().String()
	host, err := pickHost()
	if err != nil {
		return err
	}
	u := buildURL(host, fmt.Sprintf("/internal/v1/nodes/%s/config/quota/history?requestID=%s&limit=%d",
		url.PathEscape(nodeID), requestID, limit))
	var entries []model.QuotaHistoryEntry
	if err := doHttpReq(c, u, http.MethodGet, requestID, nil, &entries); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}
	if len(entries) == 0 {
		fmt.Printf("node %s has no set-quota history\n", nodeID)
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
	fmt.Fprintln(w, "OPERATOR\tTIME\tDETAIL")
	for _, e := range entries {
		fmt.Fprintf(w, "%s\t%s\t%s\n", e.Operator, formatQuotaTime(e.CreatedAt), e.Detail)
	}
	_ = w.Flush()
	return nil
}

func formatQuotaTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04:05")
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"github.com/google/uuid"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
	"github.com/urfave/cli"
)

// Command groups cluster-level configuration subcommands (internal routes).
var Command = cli.Command{
	Name:  "cluster",
	Usage: "cluster-level configuration",
	Subcommands: []cli.Command{
		{
			Name:    "quota-defaults",
			Aliases: []string{"quotadefaults"},
			Usage:   "get / set the cluster-wide paused-release-ratio default",
			Subcommands: []cli.Command{
				{
					Name:   "get",
					Usage:  "show the cluster default",
					Flags:  []cli.Flag{cli.BoolFlag{Name: "json", Usage: "print raw json response"}},
					Action: defaultsGetAction,
				},
				{
					Name:      "set",
					Usage:     "set the cluster default and fan it out to inheriting nodes",
					ArgsUsage: "<ratio in [0,1] | clear>",
					Flags: []cli.Flag{
						cli.StringFlag{Name: "operator", Value: "cli", Usage: "operator name recorded in the audit"},
						cli.BoolFlag{Name: "json", Usage: "print raw json response"},
					},
					Action: defaultsSetAction,
				},
			},
		},
	},
}

func setupClient(c *cli.Context) error {
	addrs := c.GlobalString("address")
	if addrs == "" {
		return errors.New("no server addr")
	}
	return nil
}

func defaultsGetAction(c *cli.Context) error {
	if err := setupClient(c); err != nil {
		return err
	}
	requestID := uuid.New().String()
	u := buildURL(c, "/internal/v1/cluster/quota-defaults?requestID="+requestID)
	var defaults model.ClusterQuotaDefaults
	if err := doReq(c, u, http.MethodGet, nil, &defaults); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(&defaults)
	}
	if defaults.PausedReleaseRatio == nil {
		fmt.Println("paused_release_ratio: unset")
		return nil
	}
	fmt.Printf("paused_release_ratio: %g\n", *defaults.PausedReleaseRatio)
	return nil
}

func defaultsSetAction(c *cli.Context) error {
	if err := setupClient(c); err != nil {
		return err
	}
	if c.NArg() != 1 {
		_ = cli.ShowCommandHelp(c, "set")
		return errors.New("expected exactly one argument: <ratio in [0,1] | clear>")
	}
	arg := c.Args().First()

	body := map[string]interface{}{}
	if arg != "clear" {
		v, err := strconv.ParseFloat(arg, 64)
		if err != nil {
			return fmt.Errorf("invalid ratio %q: want a number in [0,1] or \"clear\"", arg)
		}
		body["paused_release_ratio"] = v
	} else {
		body["paused_release_ratio"] = nil
	}
	data, _ := json.Marshal(body)

	requestID := uuid.New().String()
	q := url.Values{}
	q.Set("requestID", requestID)
	q.Set("operator", c.String("operator"))
	u := buildURL(c, "/internal/v1/cluster/quota-defaults?"+q.Encode())

	var out struct {
		Defaults    model.ClusterQuotaDefaults `json:"defaults"`
		Propagation model.QuotaPropagation     `json:"propagation"`
	}
	if err := doReq(c, u, http.MethodPut, data, &out); err != nil {
		return err
	}
	if c.Bool("json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(&out)
	}
	if out.Defaults.PausedReleaseRatio == nil {
		fmt.Println("cluster default cleared")
	} else {
		fmt.Printf("cluster default saved: paused_release_ratio=%g\n", *out.Defaults.PausedReleaseRatio)
	}
	if out.Propagation.Failed > 0 {
		fmt.Printf("propagation: bumped=%d pushed=%d push_failed=%d (failures converge via pull reconcile)\n",
			out.Propagation.Bumped, out.Propagation.Pushed, out.Propagation.Failed)
	} else {
		fmt.Printf("propagation: bumped=%d pushed=%d push_failed=%d\n",
			out.Propagation.Bumped, out.Propagation.Pushed, out.Propagation.Failed)
	}
	return nil
}

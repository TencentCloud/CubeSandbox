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
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	jsoniter "github.com/json-iterator/go"
	api "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	commands "github.com/tencentcloud/CubeSandbox/CubeMaster/cmd/cubemastercli/commands"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

type templateReplicaStatus struct {
	NodeID       string `json:"node_id"`
	NodeIP       string `json:"node_ip"`
	InstanceType string `json:"instance_type,omitempty"`
	Spec         string `json:"spec,omitempty"`
	SnapshotPath string `json:"snapshot_path,omitempty"`
	Status       string `json:"status"`
	Phase        string `json:"phase,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type templateResponse struct {
	RequestID                  string                      `json:"requestID,omitempty"`
	Ret                        *types.Ret                  `json:"ret,omitempty"`
	TemplateID                 string                      `json:"template_id,omitempty"`
	InstanceType               string                      `json:"instance_type,omitempty"`
	Version                    string                      `json:"version,omitempty"`
	Status                     string                      `json:"status,omitempty"`
	LastError                  string                      `json:"last_error,omitempty"`
	DisplayName                string                      `json:"display_name,omitempty"`
	CreatedAt                  string                      `json:"created_at,omitempty"`
	ImageInfo                  string                      `json:"image_info,omitempty"`
	JobID                      string                      `json:"job_id,omitempty"`
	Replicas                   []templateReplicaStatus     `json:"replicas,omitempty"`
	CreateRequest              *types.CreateCubeSandboxReq `json:"create_request,omitempty"`
	CubeEgressCABaked          bool                        `json:"cube_egress_ca_baked,omitempty"`
	CubeEgressCAFingerprint    string                      `json:"cube_egress_ca_fingerprint,omitempty"`
	CubeEgressCATargetsWritten int                         `json:"cube_egress_ca_targets_written,omitempty"`
}

type templateListResponse struct {
	Ret  *types.Ret        `json:"ret,omitempty"`
	Data []templateSummary `json:"data,omitempty"`
}

type templateSummary struct {
	TemplateID   string `json:"template_id,omitempty"`
	InstanceType string `json:"instance_type,omitempty"`
	Version      string `json:"version,omitempty"`
	Status       string `json:"status,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	ImageInfo    string `json:"image_info,omitempty"`
	JobID        string `json:"job_id,omitempty"`
}

type templateImageJobResponse struct {
	RequestID string                      `json:"requestID,omitempty"`
	Ret       *types.Ret                  `json:"ret,omitempty"`
	Job       *types.TemplateImageJobInfo `json:"job,omitempty"`
}

type templateCommitRequest struct {
	RequestID     string                      `json:"requestID,omitempty"`
	SandboxID     string                      `json:"sandbox_id,omitempty"`
	TemplateID    string                      `json:"template_id,omitempty"`
	CreateRequest *types.CreateCubeSandboxReq `json:"create_request,omitempty"`
}

type templateCommitResponse struct {
	RequestID  string     `json:"requestID,omitempty"`
	Ret        *types.Ret `json:"ret,omitempty"`
	TemplateID string     `json:"template_id,omitempty"`
	BuildID    string     `json:"build_id,omitempty"`
}

type templateBuildStatusResponse struct {
	RequestID    string     `json:"requestID,omitempty"`
	Ret          *types.Ret `json:"ret,omitempty"`
	BuildID      string     `json:"build_id,omitempty"`
	TemplateID   string     `json:"template_id,omitempty"`
	AttemptNo    int32      `json:"attempt_no,omitempty"`
	RetryOfJobID string     `json:"retry_of_job_id,omitempty"`
	Status       string     `json:"status,omitempty"`
	Progress     int        `json:"progress,omitempty"`
	Message      string     `json:"message,omitempty"`
}

type sandboxPreviewResponse struct {
	RequestID      string                      `json:"requestID,omitempty"`
	Ret            *types.Ret                  `json:"ret,omitempty"`
	APIRequest     *types.CreateCubeSandboxReq `json:"api_request,omitempty"`
	MergedRequest  *types.CreateCubeSandboxReq `json:"merged_request,omitempty"`
	CubeletRequest *api.RunCubeSandboxRequest  `json:"cubelet_request,omitempty"`
}

type templateDeleteRequest struct {
	RequestID    string `json:"RequestID,omitempty"`
	TemplateID   string `json:"template_id,omitempty"`
	InstanceType string `json:"instance_type,omitempty"`
	Sync         bool   `json:"sync,omitempty"`
}

func mergeCubeNetworkConfigFlags(c *cli.Context, existing *types.CubeNetworkConfig) *types.CubeNetworkConfig {
	hasAllowInternetAccess := c.IsSet("allow-internet-access")
	allowOut := dedupeCIDRs(c.StringSlice("allow-out-cidr"))
	denyOut := dedupeCIDRs(c.StringSlice("deny-out-cidr"))
	return mergeCubeNetworkConfigValues(existing, hasAllowInternetAccess, c.Bool("allow-internet-access"), allowOut, denyOut)
}

func mergeCubeNetworkConfigValues(existing *types.CubeNetworkConfig, hasAllowInternetAccess bool, allowInternetAccess bool, allowOut []string, denyOut []string) *types.CubeNetworkConfig {
	if !hasAllowInternetAccess && len(allowOut) == 0 && len(denyOut) == 0 {
		return existing
	}

	out := cloneCubeNetworkConfig(existing)
	if out == nil {
		out = &types.CubeNetworkConfig{}
	}
	if hasAllowInternetAccess {
		out.AllowInternetAccess = &allowInternetAccess
	}
	if len(allowOut) > 0 {
		out.AllowOut = appendUniqueCIDRs(out.AllowOut, allowOut)
	}
	if len(denyOut) > 0 {
		out.DenyOut = appendUniqueCIDRs(out.DenyOut, denyOut)
	}
	return out
}

type createFromImageExtraNetworkFlags struct {
	hasAllowInternetAccess bool
	allowInternetAccess    bool
	allowOut               []string
	denyOut                []string
}

func mergeCreateFromImageCubeNetworkConfigFlags(c *cli.Context, existing *types.CubeNetworkConfig) (*types.CubeNetworkConfig, error) {
	extra, err := parseCreateFromImageExtraNetworkFlags(c)
	if err != nil {
		return nil, err
	}
	hasAllowInternetAccess := c.IsSet("allow-internet-access") || extra.hasAllowInternetAccess
	allowInternetAccess := c.Bool("allow-internet-access")
	if extra.hasAllowInternetAccess {
		allowInternetAccess = extra.allowInternetAccess
	}
	allowOut := appendUniqueCIDRs(dedupeCIDRs(c.StringSlice("allow-out-cidr")), extra.allowOut)
	denyOut := appendUniqueCIDRs(dedupeCIDRs(c.StringSlice("deny-out-cidr")), extra.denyOut)
	return mergeCubeNetworkConfigValues(existing, hasAllowInternetAccess, allowInternetAccess, allowOut, denyOut), nil
}

func parseCreateFromImageExtraNetworkFlags(c *cli.Context) (*createFromImageExtraNetworkFlags, error) {
	extraArgs := make([]string, 0, c.NArg())
	for i := 0; i < c.NArg(); i++ {
		extraArgs = append(extraArgs, c.Args().Get(i))
	}
	if len(extraArgs) == 0 {
		return &createFromImageExtraNetworkFlags{}, nil
	}
	extra := &createFromImageExtraNetworkFlags{}
	idx := 0

	if c.IsSet("allow-internet-access") {
		if value, ok := parseBoolToken(extraArgs[idx]); ok {
			extra.hasAllowInternetAccess = true
			extra.allowInternetAccess = value
			idx++
		}
	}

	for idx < len(extraArgs) {
		arg := extraArgs[idx]
		switch {
		case arg == "--allow-out-cidr":
			idx++
			if idx >= len(extraArgs) {
				return nil, errors.New("--allow-out-cidr requires a value")
			}
			extra.allowOut = append(extra.allowOut, extraArgs[idx])
			idx++
		case strings.HasPrefix(arg, "--allow-out-cidr="):
			extra.allowOut = append(extra.allowOut, strings.TrimPrefix(arg, "--allow-out-cidr="))
			idx++
		case arg == "--deny-out-cidr":
			idx++
			if idx >= len(extraArgs) {
				return nil, errors.New("--deny-out-cidr requires a value")
			}
			extra.denyOut = append(extra.denyOut, extraArgs[idx])
			idx++
		case strings.HasPrefix(arg, "--deny-out-cidr="):
			extra.denyOut = append(extra.denyOut, strings.TrimPrefix(arg, "--deny-out-cidr="))
			idx++
		case arg == "--allow-internet-access":
			idx++
			if idx >= len(extraArgs) {
				return nil, errors.New("--allow-internet-access requires true or false when passed as a trailing argument")
			}
			value, ok := parseBoolToken(extraArgs[idx])
			if !ok {
				return nil, fmt.Errorf("invalid --allow-internet-access value %q: want true or false", extraArgs[idx])
			}
			extra.hasAllowInternetAccess = true
			extra.allowInternetAccess = value
			idx++
		case strings.HasPrefix(arg, "--allow-internet-access="):
			value, ok := parseBoolToken(strings.TrimPrefix(arg, "--allow-internet-access="))
			if !ok {
				return nil, fmt.Errorf("invalid --allow-internet-access value %q: want true or false", strings.TrimPrefix(arg, "--allow-internet-access="))
			}
			extra.hasAllowInternetAccess = true
			extra.allowInternetAccess = value
			idx++
		default:
			return nil, fmt.Errorf("unexpected positional or trailing argument %q; use --allow-internet-access=false or place bool values at the end only when explicitly supported", arg)
		}
	}

	extra.allowOut = dedupeCIDRs(extra.allowOut)
	extra.denyOut = dedupeCIDRs(extra.denyOut)
	return extra, nil
}

func parseBoolToken(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

func cloneCubeNetworkConfig(in *types.CubeNetworkConfig) *types.CubeNetworkConfig {
	if in == nil {
		return nil
	}
	out := &types.CubeNetworkConfig{
		AllowOut: append([]string(nil), in.AllowOut...),
		DenyOut:  append([]string(nil), in.DenyOut...),
	}
	if in.AllowInternetAccess != nil {
		allowInternetAccess := *in.AllowInternetAccess
		out.AllowInternetAccess = &allowInternetAccess
	}
	return out
}

func dedupeCIDRs(values []string) []string {
	return appendUniqueCIDRs(nil, values)
}

func appendUniqueCIDRs(base []string, extra []string) []string {
	seen := make(map[string]struct{}, len(base)+len(extra))
	out := append([]string(nil), base...)
	for _, cidr := range base {
		seen[cidr] = struct{}{}
	}
	for _, cidr := range extra {
		if cidr == "" {
			continue
		}
		if _, ok := seen[cidr]; ok {
			continue
		}
		seen[cidr] = struct{}{}
		out = append(out, cidr)
	}
	return out
}

func formatCubeNetworkConfig(cfg *types.CubeNetworkConfig) string {
	if cfg == nil {
		return "allow_internet_access=default(true) allow_out=[] deny_out=[] rules=0"
	}
	allow := "default(true)"
	if cfg.AllowInternetAccess != nil {
		allow = fmt.Sprintf("%t", *cfg.AllowInternetAccess)
	}
	return fmt.Sprintf("allow_internet_access=%s allow_out=%v deny_out=%v rules=%d", allow, cfg.AllowOut, cfg.DenyOut, len(cfg.Rules))
}

func formatProtoCubeNetworkConfig(cfg *api.CubeNetworkConfig) string {
	if cfg == nil {
		return "allow_internet_access=default(true) allow_out=[] deny_out=[] rules=0"
	}
	allow := "default(true)"
	if cfg.AllowInternetAccess != nil {
		allow = fmt.Sprintf("%t", cfg.GetAllowInternetAccess())
	}
	return fmt.Sprintf("allow_internet_access=%s allow_out=%v deny_out=%v rules=%d", allow, cfg.GetAllowOut(), cfg.GetDenyOut(), len(cfg.GetRules()))
}

var TemplateCommand = cli.Command{
	Name:    "template",
	Aliases: []string{"tpl"},
	Usage:   "manage cubebox templates",
	Subcommands: cli.Commands{
		TemplateCreateCommand,
		TemplateCommitCommand,
		TemplateCreateFromImageCommand,
		TemplateRedoCommand,
		TemplateDeleteCommand,
		TemplateStatusCommand,
		TemplateWatchCommand,
		TemplateBuildStatusCommand,
		TemplateBuildWatchCommand,
		TemplateListCommand,
		TemplateInfoCommand,
		TemplateRenderCommand,
	},
}

var TemplateCreateCommand = cli.Command{
	Name:  "create",
	Usage: "create template snapshots on healthy nodes",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "file, f",
			Usage: "template create request json file",
		},
		cli.StringFlag{
			Name:  "version",
			Value: "v2",
			Usage: "template version annotation",
		},
		cli.StringFlag{
			Name:  "snapshot-dir",
			Usage: "override snapshot dir passed to cubemaster",
		},
		cli.StringFlag{
			Name:  "instance-type",
			Usage: "override instance type in request body",
		},
		cli.StringSliceFlag{
			Name:  "node",
			Usage: "create template only on the specified node id or host ip; repeat to specify multiple nodes",
		},
		cli.BoolFlag{
			Name:  "allow-internet-access",
			Usage: "set allowInternetAccess on the network config for the template request",
		},
		cli.StringSliceFlag{
			Name:  "allow-out-cidr",
			Usage: "append an allowed egress CIDR to cube_network_config; repeat the flag to specify multiple CIDRs",
		},
		cli.StringSliceFlag{
			Name:  "deny-out-cidr",
			Usage: "append a denied egress CIDR to cube_network_config; repeat the flag to specify multiple CIDRs",
		},
		cli.BoolFlag{
			Name:  "json",
			Usage: "print raw json response",
		},
	},
	Action: func(c *cli.Context) error {
		filePath := c.String("file")
		if filePath == "" && c.NArg() > 0 {
			filePath = c.Args().First()
		}
		if filePath == "" {
			return errors.New("file is required")
		}

		reqBytes, err := getParams(filePath)
		if err != nil {
			return err
		}
		req := &types.CreateCubeSandboxReq{}
		if err = jsoniter.Unmarshal(reqBytes, req); err != nil {
			return err
		}
		if req.Request == nil {
			req.Request = &types.Request{}
		}
		req.RequestID = uuid.New().String()
		if req.Annotations == nil {
			req.Annotations = map[string]string{}
		}
		req.Annotations[constants.CubeAnnotationsAppSnapshotCreate] = "true"
		version := c.String("version")
		if version != "" {
			req.Annotations[constants.CubeAnnotationAppSnapshotVersion] = version
			req.Annotations[constants.CubeAnnotationAppSnapshotTemplateVersion] = version
		}
		if snapshotDir := c.String("snapshot-dir"); snapshotDir != "" {
			req.SnapshotDir = snapshotDir
		}
		if instanceType := c.String("instance-type"); instanceType != "" {
			req.InstanceType = instanceType
		}
		if scope := c.StringSlice("node"); len(scope) > 0 {
			req.DistributionScope = scope
		}
		req.CubeNetworkConfig = mergeCubeNetworkConfigFlags(c, req.CubeNetworkConfig)

		serverList = getServerAddrs(c)
		if len(serverList) == 0 {
			log.Printf("no server addr\n")
			return errors.New("no server addr")
		}
		port = c.GlobalString("port")
		host := serverList[rand.Int()%len(serverList)]
		url := fmt.Sprintf("http://%s/cube/template", net.JoinHostPort(host, port))
		body, err := jsoniter.Marshal(req)
		if err != nil {
			return err
		}
		rsp := &templateResponse{}
		if err = doHttpReq(c, url, http.MethodPost, req.RequestID, bytes.NewBuffer(body), rsp); err != nil {
			log.Printf("template create request err. %s. RequestId: %s\n", err.Error(), req.RequestID)
			return err
		}
		if rsp.Ret == nil {
			return errors.New("empty response")
		}
		if rsp.Ret.RetCode != 200 {
			log.Printf("template create failed. %s. RequestId: %s\n", rsp.Ret.RetMsg, req.RequestID)
			return errors.New(rsp.Ret.RetMsg)
		}
		if c.Bool("json") {
			commands.PrintAsJSON(rsp)
			return nil
		}
		printTemplateSummary(rsp)
		return nil
	},
}

// resolveTemplateID returns the template ID from the --template-id flag,
// falling back to the first positional argument to match docker/kubectl
// conventions (e.g. `cubemastercli tpl info <template-id>`).
func resolveTemplateID(c *cli.Context) string {
	if id := c.String("template-id"); id != "" {
		return id
	}
	if c.NArg() > 0 {
		return c.Args().First()
	}
	return ""
}

var TemplateInfoCommand = cli.Command{
	Name:      "info",
	Usage:     "show template metadata and node replicas",
	ArgsUsage: "<template-id>",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "template-id",
			Usage: "template id to query",
		},
		cli.BoolFlag{
			Name:  "include-request",
			Usage: "include the stored create request in the response",
		},
		cli.BoolFlag{
			Name:  "json",
			Usage: "print raw json response",
		},
	},
	Action: func(c *cli.Context) error {
		templateID := resolveTemplateID(c)
		if templateID == "" {
			return errors.New("template-id is required")
		}

		serverList = getServerAddrs(c)
		if len(serverList) == 0 {
			log.Printf("no server addr\n")
			return errors.New("no server addr")
		}
		port = c.GlobalString("port")
		requestID := uuid.New().String()
		host := serverList[rand.Int()%len(serverList)]
		url := fmt.Sprintf("http://%s/cube/template?template_id=%s", net.JoinHostPort(host, port), templateID)
		if c.Bool("include-request") {
			url += "&include_request=true"
		}

		rsp := &templateResponse{}
		if err := doHttpReq(c, url, http.MethodGet, requestID, nil, rsp); err != nil {
			log.Printf("template info request err. %s. RequestId: %s\n", err.Error(), requestID)
			return err
		}
		if rsp.Ret == nil {
			return errors.New("empty response")
		}
		if rsp.Ret.RetCode != 200 {
			log.Printf("template info failed. %s. RequestId: %s\n", rsp.Ret.RetMsg, requestID)
			return errors.New(rsp.Ret.RetMsg)
		}
		if c.Bool("json") {
			commands.PrintAsJSON(rsp)
			return nil
		}
		printTemplateSummary(rsp)
		return nil
	},
}

var TemplateRenderCommand = cli.Command{
	Name:  "render",
	Usage: "preview the effective sandbox request for a template",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "file, f",
			Usage: "sandbox create request json file used as preview input",
		},
		cli.StringFlag{
			Name:  "template-id",
			Usage: "template id to preview; overrides the request file annotation",
		},
		cli.BoolFlag{
			Name:  "json",
			Usage: "print raw json response",
		},
	},
	Action: func(c *cli.Context) error {
		req := &types.CreateCubeSandboxReq{}
		filePath := c.String("file")
		if filePath == "" && c.NArg() > 0 {
			filePath = c.Args().First()
		}
		if filePath != "" {
			reqBytes, err := getParams(filePath)
			if err != nil {
				return err
			}
			if err = jsoniter.Unmarshal(reqBytes, req); err != nil {
				return err
			}
		}
		if req.Request == nil {
			req.Request = &types.Request{}
		}
		req.RequestID = uuid.New().String()
		if req.Annotations == nil {
			req.Annotations = map[string]string{}
		}
		if templateID := c.String("template-id"); templateID != "" {
			req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = templateID
		}
		if req.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] == "" {
			return errors.New("template-id is required, either in request file or flag")
		}
		if constants.GetAppSnapshotVersion(req.Annotations) == "" {
			constants.SetAppSnapshotVersion(req.Annotations, "v2")
		}

		serverList = getServerAddrs(c)
		if len(serverList) == 0 {
			log.Printf("no server addr\n")
			return errors.New("no server addr")
		}
		port = c.GlobalString("port")
		host := serverList[rand.Int()%len(serverList)]
		url := fmt.Sprintf("http://%s/cube/sandbox/preview", net.JoinHostPort(host, port))

		body, err := jsoniter.Marshal(req)
		if err != nil {
			return err
		}
		rsp := &sandboxPreviewResponse{}
		if err = doHttpReq(c, url, http.MethodPost, req.RequestID, bytes.NewBuffer(body), rsp); err != nil {
			log.Printf("template render request err. %s. RequestId: %s\n", err.Error(), req.RequestID)
			return err
		}
		if rsp.Ret == nil {
			return errors.New("empty response")
		}
		if rsp.Ret.RetCode != 200 {
			log.Printf("template render failed. %s. RequestId: %s\n", rsp.Ret.RetMsg, req.RequestID)
			return errors.New(rsp.Ret.RetMsg)
		}
		if c.Bool("json") {
			commands.PrintAsJSON(rsp)
			return nil
		}
		printSandboxPreviewSummary(rsp)
		return nil
	},
}

var TemplateDeleteCommand = cli.Command{
	Name:      "delete",
	Usage:     "delete template metadata and node replicas",
	ArgsUsage: "<template-id>",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "template-id",
			Usage: "template id to delete",
		},
	},
	Action: func(c *cli.Context) error {
		templateID := resolveTemplateID(c)
		if templateID == "" {
			return errors.New("template-id is required")
		}

		serverList = getServerAddrs(c)
		if len(serverList) == 0 {
			return errors.New("no server addr")
		}
		port = c.GlobalString("port")
		requestID := uuid.New().String()
		host := serverList[rand.Int()%len(serverList)]
		url := fmt.Sprintf("http://%s/cube/template", net.JoinHostPort(host, port))

		req := &templateDeleteRequest{
			RequestID:  requestID,
			TemplateID: templateID,
		}
		body, err := jsoniter.Marshal(req)
		if err != nil {
			return err
		}

		rsp := &templateResponse{}
		if err := doHttpReq(c, url, http.MethodDelete, requestID, bytes.NewBuffer(body), rsp); err != nil {
			log.Printf("template delete request err. %s. RequestId: %s\n", err.Error(), requestID)
			return err
		}
		if rsp.Ret == nil {
			return errors.New("empty response")
		}
		if rsp.Ret.RetCode != 200 {
			log.Printf("template delete failed. %s. RequestId: %s\n", rsp.Ret.RetMsg, requestID)
			return errors.New(rsp.Ret.RetMsg)
		}
		log.Printf("template deleted: %s\n", templateID)
		return nil
	},
}

var TemplateCommitCommand = cli.Command{
	Name:  "commit",
	Usage: "commit an existing sandbox into a template",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "sandbox-id",
			Usage: "sandbox id to commit",
		},
		cli.StringFlag{
			Name:  "file, f",
			Usage: "original create_sandbox request json file",
		},
		cli.BoolFlag{
			Name:  "allow-internet-access",
			Usage: "set allowInternetAccess on the network config for the create_request",
		},
		cli.StringSliceFlag{
			Name:  "allow-out-cidr",
			Usage: "append an allowed egress CIDR to create_request.cube_network_config; repeat the flag to specify multiple CIDRs",
		},
		cli.StringSliceFlag{
			Name:  "deny-out-cidr",
			Usage: "append a denied egress CIDR to create_request.cube_network_config; repeat the flag to specify multiple CIDRs",
		},
		cli.BoolFlag{
			Name:  "detach, no-wait",
			Usage: "submit and exit immediately instead of watching the build to completion",
		},
		cli.DurationFlag{
			Name:  "interval",
			Value: defaultWatchInterval,
			Usage: "poll interval while watching the build",
		},
		cli.BoolFlag{
			Name:  "json",
			Usage: "print raw json response",
		},
	},
	Action: func(c *cli.Context) error {
		sandboxID := c.String("sandbox-id")
		filePath := c.String("file")
		if filePath == "" && c.NArg() > 0 {
			filePath = c.Args().First()
		}
		if sandboxID == "" || filePath == "" {
			return errors.New("sandbox-id and file are required")
		}

		reqBytes, err := getParams(filePath)
		if err != nil {
			return err
		}
		createReq := &types.CreateCubeSandboxReq{}
		if err = jsoniter.Unmarshal(reqBytes, createReq); err != nil {
			return err
		}
		if createReq.Request == nil {
			createReq.Request = &types.Request{}
		}
		requestID := uuid.New().String()
		createReq.RequestID = requestID
		if createReq.Annotations == nil {
			createReq.Annotations = map[string]string{}
		}
		createReq.CubeNetworkConfig = mergeCubeNetworkConfigFlags(c, createReq.CubeNetworkConfig)

		req := &templateCommitRequest{
			RequestID:     requestID,
			SandboxID:     sandboxID,
			CreateRequest: createReq,
		}

		serverList = getServerAddrs(c)
		if len(serverList) == 0 {
			return errors.New("no server addr")
		}
		port = c.GlobalString("port")
		host := serverList[rand.Int()%len(serverList)]
		url := fmt.Sprintf("http://%s/cube/sandbox/commit", net.JoinHostPort(host, port))
		body, err := jsoniter.Marshal(req)
		if err != nil {
			return err
		}

		rsp := &templateCommitResponse{}
		if err = doHttpReq(c, url, http.MethodPost, requestID, bytes.NewBuffer(body), rsp); err != nil {
			log.Printf("template commit request err. %s. RequestId: %s\n", err.Error(), requestID)
			return err
		}
		if rsp.Ret == nil {
			return errors.New("empty response")
		}
		if rsp.Ret.RetCode != 200 {
			log.Printf("template commit failed. %s. RequestId: %s\n", rsp.Ret.RetMsg, requestID)
			return errors.New(rsp.Ret.RetMsg)
		}
		if c.Bool("json") {
			commands.PrintAsJSON(rsp)
			return nil
		}
		log.Printf("template_id: %s\n", rsp.TemplateID)
		log.Printf("build_id: %s\n", rsp.BuildID)
		if detachRequested(c) || rsp.BuildID == "" {
			return nil
		}
		return runBuildWatch(c, rsp.BuildID)
	},
}

var TemplateCreateFromImageCommand = cli.Command{
	Name:  "create-from-image",
	Usage: "build ext4 rootfs from OCI image and create template asynchronously",
	Flags: []cli.Flag{
		cli.StringFlag{Name: "image", Usage: "source OCI image reference"},
		cli.StringFlag{Name: "writable-layer-size", Usage: "immutable writable layer size, e.g. 20Gi"},
		cli.StringSliceFlag{Name: "expose-port", Usage: "container port to expose for the template; repeat the flag to specify multiple ports"},
		cli.StringFlag{Name: "instance-type", Value: "cubebox", Usage: "instance type"},
		cli.StringFlag{Name: "network-type", Value: "tap", Usage: "network type"},
		cli.StringSliceFlag{Name: "node", Usage: "create template only on the specified node id or host ip; repeat to specify multiple nodes"},
		cli.BoolFlag{Name: "allow-internet-access", Usage: "set allowInternetAccess on the network config for the generated template request"},
		cli.StringSliceFlag{Name: "allow-out-cidr", Usage: "append an allowed egress CIDR to cube_network_config; repeat the flag to specify multiple CIDRs"},
		cli.StringSliceFlag{Name: "deny-out-cidr", Usage: "append a denied egress CIDR to cube_network_config; repeat the flag to specify multiple CIDRs"},
		cli.StringFlag{Name: "registry-username", Usage: "registry username"},
		cli.StringFlag{Name: "registry-password", Usage: "registry password"},

		cli.StringSliceFlag{Name: "cmd", Usage: "override container ENTRYPOINT (command); repeat for multiple elements, e.g. --cmd /bin/sh --cmd -c"},
		cli.StringSliceFlag{Name: "arg", Usage: "override container CMD (args); repeat for multiple elements"},
		cli.StringSliceFlag{Name: "env", Usage: "set environment variable, KEY=VALUE format; repeat for multiple envs"},
		cli.StringSliceFlag{Name: "dns", Usage: "set container DNS nameserver; repeat for multiple servers"},
		cli.IntFlag{Name: "probe", Usage: "enable HTTP GET probe on the specified port (e.g. --probe 9000); sets timeout_ms=30000, period_ms=500"},
		cli.StringFlag{Name: "probe-path", Value: "/health", Usage: "HTTP path for the readiness probe (default: /health); only effective when --probe is set"},
		cli.IntFlag{Name: "cpu", Value: 2000, Usage: "CPU millicores for the template container (default: 2000, i.e. 2 cores)"},
		cli.IntFlag{Name: "memory", Value: 2000, Usage: "Memory for the template container in MB (default: 2000 MB)"},
		cli.BoolTFlag{Name: "with-cube-ca", Usage: "bake the CubeEgress root CA at /etc/cube/ca/cube-root-ca.crt into the template rootfs so sandboxes trust CubeEgress's MITM. Pass --with-cube-ca=false to skip (default: true)"},
		cli.BoolFlag{Name: "detach, no-wait", Usage: "submit and exit immediately instead of watching the job to completion"},
		cli.DurationFlag{Name: "interval", Value: defaultWatchInterval, Usage: "poll interval while watching the job"},
		cli.BoolFlag{Name: "json", Usage: "print raw json response"},
	},
	Action: func(c *cli.Context) error {
		if c.String("image") == "" {
			return errors.New("image is required")
		}
		if c.String("writable-layer-size") == "" {
			return errors.New("writable-layer-size is required")
		}
		serverList = getServerAddrs(c)
		if len(serverList) == 0 {
			return errors.New("no server addr")
		}
		port = c.GlobalString("port")
		host := serverList[rand.Int()%len(serverList)]
		exposedPorts, err := parseExposePortFlags(c.StringSlice("expose-port"))
		if err != nil {
			return err
		}
		containerOverrides, err := parseContainerOverrides(c)
		if err != nil {
			return err
		}
		req := &types.CreateTemplateFromImageReq{
			Request:        &types.Request{RequestID: uuid.New().String()},
			SourceImageRef: c.String("image"),
			// TemplateID is auto-generated by normalizeTemplateImageRequest.
			WritableLayerSize:  c.String("writable-layer-size"),
			DistributionScope:  c.StringSlice("node"),
			ExposedPorts:       exposedPorts,
			InstanceType:       c.String("instance-type"),
			NetworkType:        c.String("network-type"),
			RegistryUsername:   c.String("registry-username"),
			RegistryPassword:   c.String("registry-password"),
			ContainerOverrides: containerOverrides,
		}
		// --with-cube-ca defaults true (BoolTFlag). We always materialise
		// the *bool on the wire so non-CLI callers (HTTP clients, future
		// SDKs) can still rely on `nil = server-side default`.
		withCubeCA := c.BoolT("with-cube-ca")
		req.WithCubeCA = &withCubeCA
		req.CubeNetworkConfig, err = mergeCreateFromImageCubeNetworkConfigFlags(c, req.CubeNetworkConfig)
		if err != nil {
			return err
		}
		body, err := jsoniter.Marshal(req)
		if err != nil {
			return err
		}
		url := fmt.Sprintf("http://%s/cube/template/from-image", net.JoinHostPort(host, port))
		rsp := &templateImageJobResponse{}
		if err := doHttpReq(c, url, http.MethodPost, req.RequestID, bytes.NewBuffer(body), rsp); err != nil {
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
		if detachRequested(c) || rsp.Job == nil {
			printTemplateImageJob(rsp.Job)
			return nil
		}
		log.Printf("submitted template image job: job_id=%s template_id=%s\n", rsp.Job.JobID, rsp.Job.TemplateID)
		return runImageJobWatch(c, rsp.Job.JobID)
	},
}

var TemplateRedoCommand = cli.Command{
	Name:      "redo",
	Usage:     "redo a template on all, specific, or failed nodes",
	ArgsUsage: "<template-id>",
	Flags: []cli.Flag{
		cli.StringFlag{Name: "template-id", Usage: "template id to redo"},
		cli.StringSliceFlag{Name: "node", Usage: "redo only the specified node id or host ip; repeat to specify multiple nodes"},
		cli.BoolFlag{Name: "failed-only", Usage: "redo only failed nodes"},
		cli.BoolFlag{Name: "wait", Usage: "deprecated: redo now waits by default; use --detach to opt out"},
		cli.BoolFlag{Name: "detach, no-wait", Usage: "submit and exit immediately instead of watching the redo job to completion"},
		cli.DurationFlag{Name: "interval", Value: defaultWatchInterval, Usage: "poll interval while watching the job"},
		cli.BoolFlag{Name: "json", Usage: "print raw json response"},
	},
	Action: func(c *cli.Context) error {
		templateID := resolveTemplateID(c)
		if templateID == "" {
			return errors.New("template-id is required")
		}
		serverList = getServerAddrs(c)
		if len(serverList) == 0 {
			return errors.New("no server addr")
		}
		port = c.GlobalString("port")
		host := serverList[rand.Int()%len(serverList)]
		req := &types.RedoTemplateFromImageReq{
			Request:           &types.Request{RequestID: uuid.New().String()},
			TemplateID:        templateID,
			DistributionScope: c.StringSlice("node"),
			FailedOnly:        c.Bool("failed-only"),
			Wait:              c.Bool("wait"),
		}
		body, err := jsoniter.Marshal(req)
		if err != nil {
			return err
		}
		url := fmt.Sprintf("http://%s/cube/template/redo", net.JoinHostPort(host, port))
		rsp := &templateImageJobResponse{}
		if err := doHttpReq(c, url, http.MethodPost, req.RequestID, bytes.NewBuffer(body), rsp); err != nil {
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
		if detachRequested(c) || rsp.Job == nil {
			printTemplateImageJob(rsp.Job)
			return nil
		}
		log.Printf("submitted redo job: job_id=%s template_id=%s\n", rsp.Job.JobID, rsp.Job.TemplateID)
		return runImageJobWatch(c, rsp.Job.JobID)
	},
}

var TemplateStatusCommand = cli.Command{
	Name:  "status",
	Usage: "show create-from-image job status",
	Flags: []cli.Flag{
		cli.StringFlag{Name: "job-id", Usage: "template image job id"},
		cli.BoolFlag{Name: "json", Usage: "print raw json response"},
	},
	Action: func(c *cli.Context) error {
		jobID := c.String("job-id")
		if jobID == "" {
			return errors.New("job-id is required")
		}
		rsp, err := fetchTemplateImageJob(c, jobID)
		if err != nil {
			return err
		}
		if c.Bool("json") {
			commands.PrintAsJSON(rsp)
			return nil
		}
		printTemplateImageJob(rsp.Job)
		return nil
	},
}

var TemplateWatchCommand = cli.Command{
	Name:  "watch",
	Usage: "watch create-from-image job progress until completion",
	Flags: []cli.Flag{
		cli.StringFlag{Name: "job-id", Usage: "template image job id"},
		cli.DurationFlag{Name: "interval", Value: 2 * time.Second, Usage: "poll interval"},
		cli.BoolFlag{Name: "json", Usage: "print final raw json response"},
	},
	Action: func(c *cli.Context) error {
		jobID := c.String("job-id")
		if jobID == "" {
			return errors.New("job-id is required")
		}
		return runImageJobWatch(c, jobID)
	},
}

var TemplateBuildStatusCommand = cli.Command{
	Name:  "build-status",
	Usage: "show sandbox commit build status",
	Flags: []cli.Flag{
		cli.StringFlag{Name: "build-id", Usage: "template build id"},
		cli.BoolFlag{Name: "json", Usage: "print raw json response"},
	},
	Action: func(c *cli.Context) error {
		buildID := c.String("build-id")
		if buildID == "" {
			return errors.New("build-id is required")
		}
		rsp, err := fetchTemplateBuildStatus(c, buildID)
		if err != nil {
			return err
		}
		if c.Bool("json") {
			commands.PrintAsJSON(rsp)
			return nil
		}
		printTemplateBuildStatus(rsp)
		return nil
	},
}

var TemplateBuildWatchCommand = cli.Command{
	Name:  "build-watch",
	Usage: "watch sandbox commit build status until completion",
	Flags: []cli.Flag{
		cli.StringFlag{Name: "build-id", Usage: "template build id"},
		cli.DurationFlag{Name: "interval", Value: 2 * time.Second, Usage: "poll interval"},
		cli.BoolFlag{Name: "json", Usage: "print final raw json response"},
	},
	Action: func(c *cli.Context) error {
		buildID := c.String("build-id")
		if buildID == "" {
			return errors.New("build-id is required")
		}
		return runBuildWatch(c, buildID)
	},
}

var TemplateListCommand = cli.Command{
	Name:    "list",
	Aliases: []string{"ls"},
	Usage:   "list templates",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "json",
			Usage: "print raw json response",
		},
		cli.StringFlag{
			Name:  "output,o",
			Usage: "output format, set to wide for more columns",
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
		url := fmt.Sprintf("http://%s/cube/template", net.JoinHostPort(host, port))

		rsp := &templateListResponse{}
		if err := doHttpReq(c, url, http.MethodGet, requestID, nil, rsp); err != nil {
			log.Printf("template list request err. %s. RequestId: %s\n", err.Error(), requestID)
			return err
		}
		if rsp.Ret == nil {
			return errors.New("empty response")
		}
		if rsp.Ret.RetCode != 200 {
			log.Printf("template list failed. %s. RequestId: %s\n", rsp.Ret.RetMsg, requestID)
			return errors.New(rsp.Ret.RetMsg)
		}
		if c.Bool("json") {
			commands.PrintAsJSON(rsp)
			return nil
		}
		wideOutput := strings.EqualFold(strings.TrimSpace(c.String("output")), "wide")
		w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
		tabHeader := "TEMPLATE_ID\tSTATUS\tJOB_ID\tCREATED_AT\tIMAGE_INFO"
		if wideOutput {
			tabHeader = "TEMPLATE_ID\tSTATUS\tJOB_ID\tLAST_ERROR\tCREATED_AT\tIMAGE_INFO"
		}
		fmt.Fprintln(w, tabHeader)
		for _, item := range rsp.Data {
			jobID := item.JobID
			if jobID == "" {
				jobID = "-"
			}
			if wideOutput {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					item.TemplateID, item.Status, jobID, item.LastError, item.CreatedAt, item.ImageInfo)
				continue
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				item.TemplateID, item.Status, jobID, item.CreatedAt, item.ImageInfo)
		}
		return w.Flush()
	},
}

func printTemplateSummary(rsp *templateResponse) {
	log.Printf("template_id: %s\n", rsp.TemplateID)
	if rsp.DisplayName != "" {
		log.Printf("display_name: %s\n", rsp.DisplayName)
	}
	log.Printf("instance_type: %s\n", rsp.InstanceType)
	log.Printf("version: %s\n", rsp.Version)
	log.Printf("status: %s\n", rsp.Status)
	if rsp.CreatedAt != "" {
		log.Printf("created_at: %s\n", rsp.CreatedAt)
	}
	if rsp.ImageInfo != "" {
		log.Printf("image_info: %s\n", rsp.ImageInfo)
	}
	if rsp.LastError != "" {
		log.Printf("last_error: %s\n", rsp.LastError)
	}
	if jobID := strings.TrimSpace(rsp.JobID); jobID != "" {
		log.Printf("job_id: %s\n", jobID)
	}
	// CubeEgress CA bake status. Always print so an operator can tell
	// at a glance whether sandboxes from this template will trust
	// CubeEgress's MITM certs. baked=false on a deployment that ships
	// CubeEgress is a yellow flag worth investigating (most likely a
	// distroless image that didn't have a ca-bundle to update).
	log.Printf("cube_egress_ca: baked=%t fingerprint=%s targets_written=%d\n",
		rsp.CubeEgressCABaked, fingerprintShortOrEmpty(rsp.CubeEgressCAFingerprint), rsp.CubeEgressCATargetsWritten)
	if rsp.CreateRequest != nil {
		log.Printf("cube_network_config: %s\n", formatCubeNetworkConfig(rsp.CreateRequest.CubeNetworkConfig))
	}
	w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
	fmt.Fprintln(w, "NODE_ID\tNODE_IP\tSTATUS\tPHASE\tSPEC\tERROR")
	for _, replica := range rsp.Replicas {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			replica.NodeID, replica.NodeIP, replica.Status, replica.Phase, replica.Spec, replica.ErrorMessage)
	}
	_ = w.Flush()
}

// fingerprintShortOrEmpty trims a sha256 hex fingerprint to the first
// 16 chars for printing. Full 64-char string is usable for grep but
// noisy in info output; the short form is enough for human eyeballing
// and the field gets shipped raw in the JSON --json mode.
func fingerprintShortOrEmpty(fp string) string {
	if fp == "" {
		return "(none)"
	}
	if len(fp) > 16 {
		return fp[:16]
	}
	return fp
}

func printSandboxPreviewSummary(rsp *sandboxPreviewResponse) {
	if rsp == nil {
		return
	}
	log.Printf("request_id: %s\n", rsp.RequestID)
	if rsp.APIRequest != nil {
		log.Printf("api_request: template=%s containers=%d volumes=%d network=%s\n",
			rsp.APIRequest.Annotations[constants.CubeAnnotationAppSnapshotTemplateID],
			len(rsp.APIRequest.Containers), len(rsp.APIRequest.Volumes), rsp.APIRequest.NetworkType)
		log.Printf("api_request_cube_network_config: %s\n", formatCubeNetworkConfig(rsp.APIRequest.CubeNetworkConfig))
	}
	if rsp.MergedRequest != nil {
		log.Printf("merged_request: containers=%d volumes=%d network=%s runtime=%s namespace=%s\n",
			len(rsp.MergedRequest.Containers), len(rsp.MergedRequest.Volumes), rsp.MergedRequest.NetworkType,
			rsp.MergedRequest.RuntimeHandler, rsp.MergedRequest.Namespace)
		log.Printf("merged_request_cube_network_config: %s\n", formatCubeNetworkConfig(rsp.MergedRequest.CubeNetworkConfig))
	}
	if rsp.CubeletRequest != nil {
		log.Printf("cubelet_request: containers=%d volumes=%d exposed_ports=%d\n",
			len(rsp.CubeletRequest.Containers), len(rsp.CubeletRequest.Volumes), len(rsp.CubeletRequest.ExposedPorts))
		log.Printf("cubelet_request_cube_network_config: %s\n", formatProtoCubeNetworkConfig(rsp.CubeletRequest.CubeNetworkConfig))
	}
}

func fetchTemplateImageJob(c *cli.Context, jobID string) (*templateImageJobResponse, error) {
	serverList = getServerAddrs(c)
	if len(serverList) == 0 {
		return nil, errors.New("no server addr")
	}
	port = c.GlobalString("port")
	requestID := uuid.New().String()
	host := serverList[rand.Int()%len(serverList)]
	url := fmt.Sprintf("http://%s/cube/template/from-image?job_id=%s", net.JoinHostPort(host, port), jobID)
	rsp := &templateImageJobResponse{}
	if err := doHttpReq(c, url, http.MethodGet, requestID, nil, rsp); err != nil {
		return nil, err
	}
	if rsp.Ret == nil {
		return nil, errors.New("empty response")
	}
	if rsp.Ret.RetCode != 200 {
		return nil, errors.New(rsp.Ret.RetMsg)
	}
	return rsp, nil
}

func printTemplateImageJob(job *types.TemplateImageJobInfo) {
	if job == nil {
		fmt.Println("job: <nil>")
		return
	}
	log.Printf("job_id: %s\n", job.JobID)
	log.Printf("template_id: %s\n", job.TemplateID)
	if job.AttemptNo > 0 {
		log.Printf("attempt_no: %d\n", job.AttemptNo)
	}
	if job.RetryOfJobID != "" {
		log.Printf("retry_of_job_id: %s\n", job.RetryOfJobID)
	}
	if job.Operation != "" {
		log.Printf("operation: %s\n", job.Operation)
	}
	if job.RedoMode != "" {
		log.Printf("redo_mode: %s\n", job.RedoMode)
	}
	if len(job.RedoScope) > 0 {
		log.Printf("redo_scope: %s\n", strings.Join(job.RedoScope, ","))
	}
	if job.ResumePhase != "" {
		log.Printf("resume_phase: %s\n", job.ResumePhase)
	}
	log.Printf("artifact_id: %s\n", job.ArtifactID)
	log.Printf("status: %s\n", job.Status)
	log.Printf("phase: %s\n", job.Phase)
	log.Printf("progress: %d%%\n", job.Progress)
	if job.PullTotalBytes > 0 {
		pullLine := fmt.Sprintf("pull: %s/%s", humanBytes(job.PullDownloadedBytes), humanBytes(job.PullTotalBytes))
		if job.PullSpeedBPS > 0 {
			pullLine += fmt.Sprintf(" %s/s", humanBytes(job.PullSpeedBPS))
		}
		log.Printf("%s\n", pullLine)
	}
	if job.PullTotalLayers > 0 {
		log.Printf("pull_layers: %d/%d\n", job.PullCompletedLayers, job.PullTotalLayers)
	}
	log.Printf("distribution: %d/%d ready, %d failed\n", job.ReadyNodeCount, job.ExpectedNodeCount, job.FailedNodeCount)
	if job.TemplateSpecFingerprint != "" {
		log.Printf("template_spec_fingerprint: %s\n", job.TemplateSpecFingerprint)
	}
	if job.Artifact != nil {
		log.Printf("artifact_status: %s\n", job.Artifact.Status)
		log.Printf("artifact_sha256: %s\n", job.Artifact.Ext4SHA256)
	}
	if job.TemplateStatus != "" {
		log.Printf("template_status: %s\n", job.TemplateStatus)
	}
	if job.ErrorMessage != "" {
		log.Printf("error: %s\n", job.ErrorMessage)
	}
}

func formatTemplateImageJobWatchPhase(job *types.TemplateImageJobInfo) string {
	phase := "UNKNOWN"
	if job != nil {
		if job.Status == "READY" {
			return "[7/7] READY"
		}
		if job.Phase != "" {
			phase = job.Phase
		}
	}

	phaseOrder := map[string]int{
		"PULLING":           1,
		"UNPACKING":         2,
		"BUILDING_EXT4":     3,
		"GENERATING_JSON":   4,
		"DISTRIBUTING":      5,
		"CREATING_TEMPLATE": 6,
	}
	if step, ok := phaseOrder[phase]; ok {
		return fmt.Sprintf("[%d/7] %s", step, phase)
	}
	return fmt.Sprintf("[?/7] %s", phase)
}

func formatTemplateImageJobWatchLine(job *types.TemplateImageJobInfo) string {
	if job == nil {
		return "[?/7] UNKNOWN"
	}
	parts := []string{formatTemplateImageJobWatchPhase(job)}
	parts = append(parts, fmt.Sprintf("progress=%d%%", job.Progress))
	if job.ExpectedNodeCount > 0 || job.ReadyNodeCount > 0 || job.FailedNodeCount > 0 {
		parts = append(parts, fmt.Sprintf("distribution=%d/%d ready, %d failed", job.ReadyNodeCount, job.ExpectedNodeCount, job.FailedNodeCount))
	}
	if job.TemplateID != "" {
		parts = append(parts, fmt.Sprintf("template_id=%s", job.TemplateID))
	}
	if job.JobID != "" {
		parts = append(parts, fmt.Sprintf("job_id=%s", job.JobID))
	}
	if job.ArtifactID != "" {
		parts = append(parts, fmt.Sprintf("artifact_id=%s", job.ArtifactID))
	}
	if job.ErrorMessage != "" {
		parts = append(parts, fmt.Sprintf("error=%s", job.ErrorMessage))
	}
	return strings.Join(parts, " ")
}

func formatTemplateImageJobCompletionSummary(job *types.TemplateImageJobInfo) string {
	if job == nil {
		return "template image job finished with empty response"
	}
	status := "finished"
	if job.Status == "READY" {
		status = "succeeded"
	} else if job.Status == "FAILED" {
		status = "failed"
	}
	parts := []string{fmt.Sprintf("template image job %s", status)}
	if job.TemplateID != "" {
		parts = append(parts, fmt.Sprintf("template_id=%s", job.TemplateID))
	}
	if job.JobID != "" {
		parts = append(parts, fmt.Sprintf("job_id=%s", job.JobID))
	}
	if job.ArtifactID != "" {
		parts = append(parts, fmt.Sprintf("artifact_id=%s", job.ArtifactID))
	}
	if job.ExpectedNodeCount > 0 || job.ReadyNodeCount > 0 || job.FailedNodeCount > 0 {
		parts = append(parts, fmt.Sprintf("distribution=%d/%d ready, %d failed", job.ReadyNodeCount, job.ExpectedNodeCount, job.FailedNodeCount))
	}
	if job.ErrorMessage != "" {
		parts = append(parts, fmt.Sprintf("error=%s", job.ErrorMessage))
	}
	return strings.Join(parts, " ")
}

func printTemplateImageJobWatchLine(job *types.TemplateImageJobInfo) {
	log.Print(formatTemplateImageJobWatchLine(job) + "\n")
}

func printTemplateImageJobCompletionSummary(job *types.TemplateImageJobInfo) {
	log.Print(formatTemplateImageJobCompletionSummary(job) + "\n")
}

func fetchTemplateBuildStatus(c *cli.Context, buildID string) (*templateBuildStatusResponse, error) {
	serverList = getServerAddrs(c)
	if len(serverList) == 0 {
		return nil, errors.New("no server addr")
	}
	port = c.GlobalString("port")
	requestID := uuid.New().String()
	host := serverList[rand.Int()%len(serverList)]
	url := fmt.Sprintf("http://%s/cube/template/build/%s/status", net.JoinHostPort(host, port), buildID)
	rsp := &templateBuildStatusResponse{}
	if err := doHttpReq(c, url, http.MethodGet, requestID, nil, rsp); err != nil {
		return nil, err
	}
	if rsp.Ret == nil {
		return nil, errors.New("empty response")
	}
	if rsp.Ret.RetCode != 200 {
		return nil, errors.New(rsp.Ret.RetMsg)
	}
	return rsp, nil
}

func printTemplateBuildStatus(rsp *templateBuildStatusResponse) {
	if rsp == nil {
		fmt.Println("build: <nil>")
		return
	}
	log.Printf("build_id: %s\n", rsp.BuildID)
	log.Printf("template_id: %s\n", rsp.TemplateID)
	if rsp.AttemptNo > 0 {
		log.Printf("attempt_no: %d\n", rsp.AttemptNo)
	}
	if rsp.RetryOfJobID != "" {
		log.Printf("retry_of_job_id: %s\n", rsp.RetryOfJobID)
	}
	log.Printf("status: %s\n", rsp.Status)
	log.Printf("progress: %d%%\n", rsp.Progress)
	if rsp.Message != "" {
		log.Printf("message: %s\n", rsp.Message)
	}
}

func parseExposePortFlags(values []string) ([]int32, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]int32, 0, len(values))
	for _, value := range values {
		port, err := strconv.ParseInt(value, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid expose-port %q: %w", value, err)
		}
		out = append(out, int32(port))
	}
	return out, nil
}

func parseContainerOverrides(c *cli.Context) (*types.ContainerOverrides, error) {
	cmds := c.StringSlice("cmd")
	args := c.StringSlice("arg")
	rawEnvs := c.StringSlice("env")
	dnsServers := c.StringSlice("dns")
	probePort := c.Int("probe")
	cpuMillicores := c.Int("cpu")
	memoryMB := c.Int("memory")

	if len(cmds) == 0 && len(args) == 0 && len(rawEnvs) == 0 && len(dnsServers) == 0 && probePort == 0 && !c.IsSet("cpu") && !c.IsSet("memory") {
		return nil, nil
	}

	overrides := &types.ContainerOverrides{}
	if len(cmds) > 0 {
		overrides.Command = cmds
	}
	if len(args) > 0 {
		overrides.Args = args
	}
	for _, kv := range rawEnvs {
		idx := strings.IndexByte(kv, '=')
		if idx < 0 {
			return nil, fmt.Errorf("invalid env %q: expected KEY=VALUE format", kv)
		}
		overrides.Envs = append(overrides.Envs, &types.KeyValue{
			Key:   kv[:idx],
			Value: kv[idx+1:],
		})
	}
	if len(dnsServers) > 0 {
		for _, dnsServer := range dnsServers {
			if dnsServer == "" || net.ParseIP(dnsServer) == nil {
				return nil, fmt.Errorf("invalid dns server %q", dnsServer)
			}
		}
		overrides.DnsConfig = &types.DNSConfig{Servers: dnsServers}
	}
	if c.IsSet("cpu") || c.IsSet("memory") {
		overrides.Resources = &types.Resource{
			Cpu: fmt.Sprintf("%dm", cpuMillicores),
			Mem: fmt.Sprintf("%dMi", memoryMB),
		}
	}
	if probePort > 0 {
		probePath := c.String("probe-path")
		if probePath == "" {
			probePath = "/health"
		}
		host := ""
		overrides.Probe = &types.Probe{
			ProbeHandler: &types.ProbeHandler{
				HttpGet: &types.HTTPGetAction{
					Path: &probePath,
					Port: int32(probePort),
					Host: &host,
				},
			},
			TimeoutMs:        30000,
			PeriodMs:         500,
			FailureThreshold: 60,
			SuccessThreshold: 1,
		}
	}
	return overrides, nil
}

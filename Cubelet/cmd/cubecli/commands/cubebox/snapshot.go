// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/urfave/cli/v2"

	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

var Snapshot = &cli.Command{
	Name:  "snapshot",
	Usage: "create AGS app snapshot from cubebox template",
	UsageText: `cubecli cubebox snapshot [options] <request.json>

This command calls the AppSnapshot gRPC API which performs:
  1. Create a cubebox sandbox based on the request configuration
  2. Execute cube-runtime snapshot to create the app snapshot
  3. Clean up the cubebox after snapshot creation (success or failure)

Example:
  cubecli cubebox snapshot ./cubebox_request.json
  cubecli cubebox snapshot --snapshot-dir /custom/path ./cubebox_request.json
  cubecli cubebox snapshot --json ./cubebox_request.json`,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "snapshot-dir",
			Usage: "custom snapshot directory",
			Value: constants.DefaultSnapshotDir,
		},
		&cli.BoolFlag{
			Name:  "json",
			Usage: "output result in JSON format",
			Value: false,
		},
	},
	Action: snapshotAction,
}

type SnapshotResult struct {
	Success      bool   `json:"success"`
	RequestID    string `json:"request_id"`
	SandboxID    string `json:"sandbox_id,omitempty"`
	TemplateID   string `json:"template_id,omitempty"`
	SnapshotPath string `json:"snapshot_path,omitempty"`
	Error        string `json:"error,omitempty"`
	Duration     string `json:"duration,omitempty"`
}

func snapshotAction(cliCtx *cli.Context) error {
	args := cliCtx.Args().Slice()
	if len(args) < 1 {
		return fmt.Errorf("please provide a request json file path")
	}

	requestFile := args[0]
	snapshotDir := cliCtx.String("snapshot-dir")
	jsonOutput := cliCtx.Bool("json")

	req, err := readRunSandboxReqFromFile(requestFile)
	if err != nil {
		if jsonOutput {
			printJSONResult(&SnapshotResult{
				Success: false,
				Error:   fmt.Sprintf("failed to read request file: %v", err),
			})
			return nil
		}
		return fmt.Errorf("failed to read request file: %v", err)
	}

	if req.RequestID == "" {
		req.RequestID = uuid.New().String()
	}

	startTime := time.Now()

	if !jsonOutput {
		printHeader("AppSnapshot")
		printKeyValue("Request ID", req.RequestID)
		printKeyValue("Snapshot Dir", snapshotDir)
		if req.Annotations != nil {
			if templateID := req.Annotations["cube.master.appsnapshot.template.id"]; templateID != "" {
				printKeyValue("Template ID", templateID)
			}
		}
		printSeparator()
	}

	conn, grpcCtx, cancel, err := commands.NewGrpcConn(cliCtx)
	if err != nil {
		if jsonOutput {
			printJSONResult(&SnapshotResult{
				Success:   false,
				RequestID: req.RequestID,
				Error:     fmt.Sprintf("failed to create grpc connection: %v", err),
			})
			return nil
		}
		return fmt.Errorf("failed to create grpc connection: %v", err)
	}
	defer conn.Close()
	defer cancel()

	client := cubebox.NewCubeboxMgrClient(conn)

	probeTimeout := calculateProbeTimeout(req)

	grpcTimeout := probeTimeout + 5*time.Minute
	grpcCtx, grpcCancel := context.WithTimeout(grpcCtx, grpcTimeout)
	defer grpcCancel()

	if !jsonOutput {
		printStep("Calling AppSnapshot API...")
		printKeyValue("Timeout", grpcTimeout.String())
	}

	appSnapshotReq := &cubebox.AppSnapshotRequest{
		CreateRequest: req,
		SnapshotDir:   snapshotDir,
	}

	resp, err := client.AppSnapshot(grpcCtx, appSnapshotReq)
	duration := time.Since(startTime)

	if err != nil {
		if jsonOutput {
			printJSONResult(&SnapshotResult{
				Success:   false,
				RequestID: req.RequestID,
				Error:     fmt.Sprintf("AppSnapshot API call failed: %v", err),
				Duration:  duration.String(),
			})
			return nil
		}
		printError("AppSnapshot API call failed: %v", err)
		return fmt.Errorf("AppSnapshot API call failed: %v", err)
	}

	if resp.Ret.RetCode != errorcode.ErrorCode_Success {
		if jsonOutput {
			printJSONResult(&SnapshotResult{
				Success:    false,
				RequestID:  resp.RequestID,
				SandboxID:  resp.SandboxID,
				TemplateID: resp.TemplateID,
				Error:      resp.Ret.RetMsg,
				Duration:   duration.String(),
			})
			return nil
		}
		printError("AppSnapshot failed: %s (code: %v)", resp.Ret.RetMsg, resp.Ret.RetCode)
		return fmt.Errorf("AppSnapshot failed: %s", resp.Ret.RetMsg)
	}

	if jsonOutput {
		printJSONResult(&SnapshotResult{
			Success:      true,
			RequestID:    resp.RequestID,
			SandboxID:    resp.SandboxID,
			TemplateID:   resp.TemplateID,
			SnapshotPath: resp.SnapshotPath,
			Duration:     duration.String(),
		})
		return nil
	}

	printSeparator()
	printSuccess("AppSnapshot completed successfully!")
	printSeparator()
	printKeyValue("Request ID", resp.RequestID)
	printKeyValue("Sandbox ID", resp.SandboxID)
	printKeyValue("Template ID", resp.TemplateID)
	printKeyValue("Snapshot Path", resp.SnapshotPath)
	printKeyValue("Duration", duration.String())
	printSeparator()

	return nil
}

func calculateProbeTimeout(req *cubebox.RunCubeSandboxRequest) time.Duration {
	var totalMs int32 = 0

	for _, container := range req.GetContainers() {
		if probe := container.GetProbe(); probe != nil {
			probeTime := probe.GetTimeoutMs()
			if probeTime > totalMs {
				totalMs = probeTime
			}
		}
	}

	if totalMs < 120*1000 {
		totalMs = 120 * 1000
	}

	return time.Duration(totalMs) * time.Millisecond
}

func printJSONResult(result *SnapshotResult) {
	data, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(data))
}

func printHeader(title string) {
	line := strings.Repeat("=", 50)
	fmt.Printf("\n%s\n", line)
	fmt.Printf("  %s\n", title)
	fmt.Printf("%s\n\n", line)
}

func printSeparator() {
	fmt.Println(strings.Repeat("-", 50))
}

func printKeyValue(key, value string) {
	fmt.Printf("  %-15s: %s\n", key, value)
}

func printStep(format string, args ...interface{}) {
	fmt.Printf(">> %s\n", fmt.Sprintf(format, args...))
}

func printSuccess(format string, args ...interface{}) {
	fmt.Printf("✓ %s\n", fmt.Sprintf(format, args...))
}

func printError(format string, args ...interface{}) {
	fmt.Printf("✗ %s\n", fmt.Sprintf(format, args...))
}

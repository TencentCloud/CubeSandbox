// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/urfave/cli/v2"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

// DebugCommitSandbox is a temporary DEBUG ONLY command for validating the
// phase-4 Cubelet CommitSandbox cubecow path before the Master/API control
// plane is fully wired.
var DebugCommitSandbox = &cli.Command{
	Name:  "debug-commit",
	Usage: "DEBUG ONLY: commit a running sandbox by calling Cubelet CommitSandbox directly",
	UsageText: `cubecli cubebox debug-commit --sandbox-id <sandbox_id> --template-id <snapshot_id> [options]

DEBUG ONLY:
  This command is only for phase-4 cubecow snapshot validation. It bypasses
  CubeMaster/CubeAPI metadata state machines and calls Cubelet CommitSandbox
  directly, so do not use it as a production snapshot entrypoint.

Examples:
  cubecli cubebox debug-commit --sandbox-id sb1 --template-id snap1
  cubecli cubebox debug-commit --sandbox-id sb1 --template-id snap1 --snapshot-dir /tmp/cube-snapshot
  cubecli cubebox debug-commit --sandbox-id sb1 --template-id snap1 --json`,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:     "sandbox-id",
			Usage:    "running sandbox ID to commit",
			Required: true,
		},
		&cli.StringFlag{
			Name:     "template-id",
			Usage:    "snapshot/template ID used for tpl-<id>-rootfs and tpl-<id>-memory",
			Required: true,
		},
		&cli.StringFlag{
			Name:  "snapshot-dir",
			Usage: "custom snapshot metadata directory",
			Value: constants.DefaultSnapshotDir,
		},
		&cli.BoolFlag{
			Name:  "json",
			Usage: "output result in JSON format",
			Value: false,
		},
	},
	Action: debugCommitSandboxAction,
}

type debugCommitSandboxResult struct {
	Success         bool   `json:"success"`
	RequestID       string `json:"request_id"`
	SandboxID       string `json:"sandbox_id"`
	TemplateID      string `json:"template_id"`
	SnapshotPath    string `json:"snapshot_path,omitempty"`
	RootfsVol       string `json:"rootfs_vol,omitempty"`
	MemoryVol       string `json:"memory_vol,omitempty"`
	RootfsKind      string `json:"rootfs_kind,omitempty"`
	MemoryKind      string `json:"memory_kind,omitempty"`
	RootfsDev       string `json:"rootfs_dev,omitempty"`
	MemoryDev       string `json:"memory_dev,omitempty"`
	RootfsSizeBytes uint64 `json:"rootfs_size_bytes,omitempty"`
	Error           string `json:"error,omitempty"`
	Duration        string `json:"duration,omitempty"`
}

func debugCommitSandboxAction(cliCtx *cli.Context) error {
	requestID := uuid.NewString()
	sandboxID := cliCtx.String("sandbox-id")
	templateID := cliCtx.String("template-id")
	snapshotDir := cliCtx.String("snapshot-dir")
	jsonOutput := cliCtx.Bool("json")
	startTime := time.Now()

	if !jsonOutput {
		printHeader("DEBUG CommitSandbox")
		printStep("This command is DEBUG ONLY and bypasses CubeMaster/CubeAPI.")
		printKeyValue("Request ID", requestID)
		printKeyValue("Sandbox ID", sandboxID)
		printKeyValue("Template ID", templateID)
		printKeyValue("Snapshot Dir", snapshotDir)
		printSeparator()
	}

	conn, grpcCtx, cancel, err := commands.NewGrpcConn(cliCtx)
	if err != nil {
		result := &debugCommitSandboxResult{
			Success:    false,
			RequestID:  requestID,
			SandboxID:  sandboxID,
			TemplateID: templateID,
			Error:      fmt.Sprintf("failed to create grpc connection: %v", err),
			Duration:   time.Since(startTime).String(),
		}
		if jsonOutput {
			printDebugCommitJSONResult(result)
			return nil
		}
		return errors.New(result.Error)
	}
	defer conn.Close()
	defer cancel()

	client := cubebox.NewCubeboxMgrClient(conn)
	grpcCtx, grpcCancel := context.WithTimeout(grpcCtx, cliCtx.Duration("timeout"))
	defer grpcCancel()

	resp, err := client.CommitSandbox(grpcCtx, &cubebox.CommitSandboxRequest{
		RequestID:   requestID,
		SandboxID:   sandboxID,
		TemplateID:  templateID,
		SnapshotDir: snapshotDir,
	})
	duration := time.Since(startTime)
	if err != nil {
		result := &debugCommitSandboxResult{
			Success:    false,
			RequestID:  requestID,
			SandboxID:  sandboxID,
			TemplateID: templateID,
			Error:      fmt.Sprintf("CommitSandbox API call failed: %v", err),
			Duration:   duration.String(),
		}
		if jsonOutput {
			printDebugCommitJSONResult(result)
			return nil
		}
		printError("%s", result.Error)
		return errors.New(result.Error)
	}

	result := debugCommitSandboxResultFromResponse(resp, duration)
	ret := resp.GetRet()
	if ret == nil || ret.GetRetCode() != errorcode.ErrorCode_Success {
		if ret != nil {
			result.Error = ret.GetRetMsg()
		} else {
			result.Error = "CommitSandbox returned empty ret"
		}
		result.Success = false
		if jsonOutput {
			printDebugCommitJSONResult(result)
			return nil
		}
		printError("CommitSandbox failed: %s", result.Error)
		return fmt.Errorf("CommitSandbox failed: %s", result.Error)
	}

	result.Success = true
	if jsonOutput {
		printDebugCommitJSONResult(result)
		return nil
	}

	printSeparator()
	printSuccess("CommitSandbox completed successfully!")
	printSeparator()
	printKeyValue("Request ID", result.RequestID)
	printKeyValue("Sandbox ID", result.SandboxID)
	printKeyValue("Template ID", result.TemplateID)
	printKeyValue("Snapshot Path", result.SnapshotPath)
	printKeyValue("Rootfs Vol", result.RootfsVol)
	printKeyValue("Memory Vol", result.MemoryVol)
	printKeyValue("Rootfs Kind", result.RootfsKind)
	printKeyValue("Memory Kind", result.MemoryKind)
	printKeyValue("Rootfs Dev", result.RootfsDev)
	printKeyValue("Memory Dev", result.MemoryDev)
	printKeyValue("Rootfs Size Bytes", fmt.Sprintf("%d", result.RootfsSizeBytes))
	printKeyValue("Duration", result.Duration)
	printSeparator()

	return nil
}

func debugCommitSandboxResultFromResponse(resp *cubebox.CommitSandboxResponse, duration time.Duration) *debugCommitSandboxResult {
	if resp == nil {
		return &debugCommitSandboxResult{
			Success:  false,
			Error:    "CommitSandbox returned nil response",
			Duration: duration.String(),
		}
	}
	return &debugCommitSandboxResult{
		RequestID:       resp.GetRequestID(),
		SandboxID:       resp.GetSandboxID(),
		TemplateID:      resp.GetTemplateID(),
		SnapshotPath:    resp.GetSnapshotPath(),
		RootfsVol:       resp.GetRootfsVol(),
		MemoryVol:       resp.GetMemoryVol(),
		RootfsKind:      resp.GetRootfsKind(),
		MemoryKind:      resp.GetMemoryKind(),
		RootfsDev:       resp.GetRootfsDev(),
		MemoryDev:       resp.GetMemoryDev(),
		RootfsSizeBytes: resp.GetRootfsSizeBytes(),
		Duration:        duration.String(),
	}
}

func printDebugCommitJSONResult(result *debugCommitSandboxResult) {
	data, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(data))
}

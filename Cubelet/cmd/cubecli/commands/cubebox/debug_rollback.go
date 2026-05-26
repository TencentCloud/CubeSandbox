// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/urfave/cli/v2"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
)

// DebugRollbackSandbox is a temporary DEBUG ONLY command for validating the
// phase-5 Cubelet RollbackSandbox cubecow path before the Master/API control
// plane is fully wired.
var DebugRollbackSandbox = &cli.Command{
	Name:  "debug-rollback",
	Usage: "DEBUG ONLY: rollback a running sandbox by calling Cubelet RollbackSandbox directly",
	UsageText: `cubecli cubebox debug-rollback --sandbox-id <sandbox_id> --snapshot-id <snapshot_id> --rootfs-vol <tpl-snap-rootfs> --memory-vol <tpl-snap-memory> --meta-dir <snapshot_path> --new-gen <N> [options]

DEBUG ONLY:
  This command is only for phase-5 cubecow rollback validation. It bypasses
  CubeMaster/CubeAPI metadata state machines and calls Cubelet RollbackSandbox
  directly, so do not use it as a production rollback entrypoint.

Examples:
  cubecli cubebox debug-rollback --sandbox-id sb1 --snapshot-id snap1 --rootfs-vol tpl-snap1-rootfs --memory-vol tpl-snap1-memory --meta-dir /var/lib/cube/snapshots/snap1 --new-gen 1
  cubecli cubebox debug-rollback --from-commit-result ./commit.json --new-gen 1
  cubecli cubebox debug-rollback --from-commit-result ./commit.json --new-gen 1 --json`,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "from-commit-result",
			Usage: "read snapshot_id/rootfs_vol/memory_vol/meta_dir/desired_size from debug-commit JSON output",
		},
		&cli.StringFlag{
			Name:  "sandbox-id",
			Usage: "running sandbox ID to rollback",
		},
		&cli.StringFlag{
			Name:  "snapshot-id",
			Usage: "logical snapshot/template ID",
		},
		&cli.StringFlag{
			Name:  "rootfs-vol",
			Usage: "authoritative cubecow snapshot rootfs object name",
		},
		&cli.StringFlag{
			Name:  "memory-vol",
			Usage: "authoritative cubecow snapshot memory object name",
		},
		&cli.StringFlag{
			Name:  "meta-dir",
			Usage: "snapshot metadata directory",
		},
		&cli.UintFlag{
			Name:  "new-gen",
			Usage: "new sandbox rootfs generation to derive",
		},
		&cli.Uint64Flag{
			Name:  "desired-size",
			Usage: "minimum rootfs size after deriving the new generation; defaults to debug-commit rootfs_size_bytes when --from-commit-result is used",
		},
		&cli.BoolFlag{
			Name:  "json",
			Usage: "output result in JSON format",
			Value: false,
		},
	},
	Action: debugRollbackSandboxAction,
}

type debugRollbackSandboxInput struct {
	SandboxID       string `json:"sandbox_id"`
	SnapshotID      string `json:"snapshot_id"`
	RootfsVol       string `json:"rootfs_vol"`
	MemoryVol       string `json:"memory_vol"`
	MetaDir         string `json:"meta_dir"`
	NewGen          uint32 `json:"new_gen"`
	DesiredSize     uint64 `json:"desired_size"`
	RootfsSizeBytes uint64 `json:"rootfs_size_bytes"`
	SnapshotPath    string `json:"snapshot_path"`
	TemplateID      string `json:"template_id"`
}

type debugRollbackSandboxResult struct {
	Success          bool   `json:"success"`
	RequestID        string `json:"request_id"`
	SandboxID        string `json:"sandbox_id"`
	SnapshotID       string `json:"snapshot_id"`
	RootfsVol        string `json:"rootfs_vol,omitempty"`
	RootfsKind       string `json:"rootfs_kind,omitempty"`
	RootfsDev        string `json:"rootfs_dev,omitempty"`
	NewGen           uint32 `json:"new_gen,omitempty"`
	OldRootfsVol     string `json:"old_rootfs_vol,omitempty"`
	OldRootfsDeleted bool   `json:"old_rootfs_deleted,omitempty"`
	Error            string `json:"error,omitempty"`
	Duration         string `json:"duration,omitempty"`
}

func debugRollbackSandboxAction(cliCtx *cli.Context) error {
	input, err := readDebugRollbackInput(cliCtx)
	if err != nil {
		return err
	}
	jsonOutput := cliCtx.Bool("json")
	requestID := uuid.NewString()
	startTime := time.Now()

	if !jsonOutput {
		printHeader("DEBUG RollbackSandbox")
		printStep("This command is DEBUG ONLY and bypasses CubeMaster/CubeAPI.")
		printKeyValue("Request ID", requestID)
		printKeyValue("Sandbox ID", input.SandboxID)
		printKeyValue("Snapshot ID", input.SnapshotID)
		printKeyValue("Rootfs Vol", input.RootfsVol)
		printKeyValue("Memory Vol", input.MemoryVol)
		printKeyValue("Meta Dir", input.MetaDir)
		printKeyValue("New Gen", fmt.Sprintf("%d", input.NewGen))
		printKeyValue("Desired Size", fmt.Sprintf("%d", input.DesiredSize))
		printSeparator()
	}

	conn, grpcCtx, cancel, err := commands.NewGrpcConn(cliCtx)
	if err != nil {
		result := &debugRollbackSandboxResult{
			Success:    false,
			RequestID:  requestID,
			SandboxID:  input.SandboxID,
			SnapshotID: input.SnapshotID,
			Error:      fmt.Sprintf("failed to create grpc connection: %v", err),
			Duration:   time.Since(startTime).String(),
		}
		if jsonOutput {
			printDebugRollbackJSONResult(result)
			return nil
		}
		return errors.New(result.Error)
	}
	defer conn.Close()
	defer cancel()

	client := cubebox.NewCubeboxMgrClient(conn)
	grpcCtx, grpcCancel := context.WithTimeout(grpcCtx, cliCtx.Duration("timeout"))
	defer grpcCancel()

	resp, err := client.RollbackSandbox(grpcCtx, &cubebox.RollbackSandboxRequest{
		RequestID:   requestID,
		SandboxID:   input.SandboxID,
		SnapshotID:  input.SnapshotID,
		RootfsVol:   input.RootfsVol,
		MemoryVol:   input.MemoryVol,
		MetaDir:     input.MetaDir,
		NewGen:      input.NewGen,
		DesiredSize: input.DesiredSize,
	})
	duration := time.Since(startTime)
	if err != nil {
		result := &debugRollbackSandboxResult{
			Success:    false,
			RequestID:  requestID,
			SandboxID:  input.SandboxID,
			SnapshotID: input.SnapshotID,
			Error:      fmt.Sprintf("RollbackSandbox API call failed: %v", err),
			Duration:   duration.String(),
		}
		if jsonOutput {
			printDebugRollbackJSONResult(result)
			return nil
		}
		printError("%s", result.Error)
		return errors.New(result.Error)
	}

	result := debugRollbackSandboxResultFromResponse(resp, duration)
	ret := resp.GetRet()
	if ret == nil || ret.GetRetCode() != errorcode.ErrorCode_Success {
		if ret != nil {
			result.Error = ret.GetRetMsg()
		} else {
			result.Error = "RollbackSandbox returned empty ret"
		}
		result.Success = false
		if jsonOutput {
			printDebugRollbackJSONResult(result)
			return nil
		}
		printError("RollbackSandbox failed: %s", result.Error)
		return fmt.Errorf("RollbackSandbox failed: %s", result.Error)
	}

	result.Success = true
	if jsonOutput {
		printDebugRollbackJSONResult(result)
		return nil
	}

	printSeparator()
	printSuccess("RollbackSandbox completed successfully!")
	printSeparator()
	printKeyValue("Request ID", result.RequestID)
	printKeyValue("Sandbox ID", result.SandboxID)
	printKeyValue("Snapshot ID", result.SnapshotID)
	printKeyValue("Rootfs Vol", result.RootfsVol)
	printKeyValue("Rootfs Kind", result.RootfsKind)
	printKeyValue("Rootfs Dev", result.RootfsDev)
	printKeyValue("New Gen", fmt.Sprintf("%d", result.NewGen))
	printKeyValue("Old Rootfs Vol", result.OldRootfsVol)
	printKeyValue("Old Rootfs Deleted", fmt.Sprintf("%t", result.OldRootfsDeleted))
	printKeyValue("Duration", result.Duration)
	printSeparator()

	return nil
}

func readDebugRollbackInput(cliCtx *cli.Context) (*debugRollbackSandboxInput, error) {
	input := &debugRollbackSandboxInput{}
	if path := cliCtx.String("from-commit-result"); path != "" {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read commit result %s: %w", path, err)
		}
		if err := json.Unmarshal(body, input); err != nil {
			return nil, fmt.Errorf("failed to parse commit result %s: %w", path, err)
		}
		input.SnapshotID = firstNonEmpty(input.SnapshotID, input.TemplateID)
		input.MetaDir = firstNonEmpty(input.MetaDir, input.SnapshotPath)
		if input.DesiredSize == 0 {
			input.DesiredSize = input.RootfsSizeBytes
		}
	}

	input.SandboxID = firstNonEmpty(cliCtx.String("sandbox-id"), input.SandboxID)
	input.SnapshotID = firstNonEmpty(cliCtx.String("snapshot-id"), input.SnapshotID)
	input.RootfsVol = firstNonEmpty(cliCtx.String("rootfs-vol"), input.RootfsVol)
	input.MemoryVol = firstNonEmpty(cliCtx.String("memory-vol"), input.MemoryVol)
	input.MetaDir = firstNonEmpty(cliCtx.String("meta-dir"), input.MetaDir)
	if cliCtx.IsSet("new-gen") {
		input.NewGen = uint32(cliCtx.Uint("new-gen"))
	}
	if cliCtx.IsSet("desired-size") {
		input.DesiredSize = cliCtx.Uint64("desired-size")
	}

	if input.SandboxID == "" {
		return nil, fmt.Errorf("sandbox-id is required")
	}
	if input.SnapshotID == "" {
		return nil, fmt.Errorf("snapshot-id is required")
	}
	if input.RootfsVol == "" {
		return nil, fmt.Errorf("rootfs-vol is required")
	}
	if input.MemoryVol == "" {
		return nil, fmt.Errorf("memory-vol is required")
	}
	if input.MetaDir == "" {
		return nil, fmt.Errorf("meta-dir is required")
	}
	if input.NewGen == 0 {
		return nil, fmt.Errorf("new-gen is required")
	}
	return input, nil
}

func debugRollbackSandboxResultFromResponse(resp *cubebox.RollbackSandboxResponse, duration time.Duration) *debugRollbackSandboxResult {
	if resp == nil {
		return &debugRollbackSandboxResult{
			Success:  false,
			Error:    "RollbackSandbox returned nil response",
			Duration: duration.String(),
		}
	}
	return &debugRollbackSandboxResult{
		RequestID:        resp.GetRequestID(),
		SandboxID:        resp.GetSandboxID(),
		SnapshotID:       resp.GetSnapshotID(),
		RootfsVol:        resp.GetRootfsVol(),
		RootfsKind:       resp.GetRootfsKind(),
		RootfsDev:        resp.GetRootfsDev(),
		NewGen:           resp.GetNewGen(),
		OldRootfsVol:     resp.GetOldRootfsVol(),
		OldRootfsDeleted: resp.GetOldRootfsDeleted(),
		Duration:         duration.String(),
	}
}

func printDebugRollbackJSONResult(result *debugRollbackSandboxResult) {
	data, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(data))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

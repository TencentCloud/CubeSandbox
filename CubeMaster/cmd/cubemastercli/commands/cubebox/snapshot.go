// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	jsoniter "github.com/json-iterator/go"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/cmd/cubemastercli/commands"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

// snapshotCreateRequest matches the slimmed-down server body. The caller no
// longer needs to ship the original create_sandbox JSON because master keeps a
// canonical copy in its sandbox spec store.
type snapshotCreateRequest struct {
	RequestID   string `json:"requestID,omitempty"`
	SandboxID   string `json:"sandbox_id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

type snapshotResponse struct {
	*types.Res
	Snapshot  *snapshotResource  `json:"snapshot,omitempty"`
	Operation *operationResource `json:"operation,omitempty"`
}

type snapshotListResponse struct {
	*types.Res
	Data []*snapshotResource `json:"data,omitempty"`
}

type snapshotResource struct {
	SnapshotID                string                      `json:"snapshot_id,omitempty"`
	InstanceType              string                      `json:"instance_type,omitempty"`
	Version                   string                      `json:"version,omitempty"`
	Status                    string                      `json:"status,omitempty"`
	DisplayName               string                      `json:"display_name,omitempty"`
	OriginSandboxID           string                      `json:"origin_sandbox_id,omitempty"`
	OriginNodeID              string                      `json:"origin_node_id,omitempty"`
	StorageBackend            string                      `json:"storage_backend,omitempty"`
	Retain                    bool                        `json:"retain,omitempty"`
	RootfsSizeBytesAtSnapshot uint64                      `json:"rootfs_size_bytes_at_snapshot,omitempty"`
	LastError                 string                      `json:"last_error,omitempty"`
	CreatedAt                 string                      `json:"created_at,omitempty"`
	RuntimeRefCount           int64                       `json:"runtime_ref_count,omitempty"`
	RuntimeRefSandboxes       []string                    `json:"runtime_ref_sandboxes,omitempty"`
	Replicas                  []replicaStatus             `json:"replicas,omitempty"`
	CreateRequest             *types.CreateCubeSandboxReq `json:"create_request,omitempty"`
}

type replicaStatus struct {
	NodeID       string `json:"node_id,omitempty"`
	NodeIP       string `json:"node_ip,omitempty"`
	Status       string `json:"status,omitempty"`
	Phase        string `json:"phase,omitempty"`
	Spec         string `json:"spec,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type operationResponse struct {
	*types.Res
	Operation *operationResource `json:"operation,omitempty"`
}

type operationResource struct {
	OperationID  string `json:"operation_id,omitempty"`
	SnapshotID   string `json:"snapshot_id,omitempty"`
	SandboxID    string `json:"sandbox_id,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
	Operation    string `json:"operation,omitempty"`
	Status       string `json:"status,omitempty"`
	Phase        string `json:"phase,omitempty"`
	Progress     int32  `json:"progress,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	AttemptNo    int32  `json:"attempt_no,omitempty"`
	RetryOfJobID string `json:"retry_of_job_id,omitempty"`
	ResourceType string `json:"resource_type,omitempty"`
	ResourceID   string `json:"resource_id,omitempty"`
}

type snapshotStorageStatusResponse struct {
	*types.Res
	Data []*snapshotStorageStatus `json:"data,omitempty"`
}

type snapshotStorageStatus struct {
	NodeID        string `json:"node_id,omitempty"`
	NodeIP        string `json:"node_ip,omitempty"`
	UsagePct      uint64 `json:"usage_pct,omitempty"`
	Mode          string `json:"mode,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastUpdatedAt int64  `json:"last_updated_at,omitempty"`
}

type rollbackRequest struct {
	RequestID    string `json:"requestID,omitempty"`
	SandboxID    string `json:"sandbox_id,omitempty"`
	SnapshotID   string `json:"snapshot_id,omitempty"`
	InstanceType string `json:"instance_type,omitempty"`
}

var SnapshotCommand = cli.Command{
	Name:  "snapshot",
	Usage: "manage runtime snapshots",
	Subcommands: cli.Commands{
		SnapshotCreateCommand,
		SnapshotListCommand,
		SnapshotInfoCommand,
		SnapshotDeleteCommand,
	},
}

var SnapshotCreateCommand = cli.Command{
	Name:  "create",
	Usage: "create a snapshot from an existing sandbox",
	Flags: []cli.Flag{
		cli.StringFlag{Name: "sandbox-id", Usage: "sandbox id to snapshot"},
		cli.StringFlag{Name: "display-name", Usage: "snapshot display name"},
		cli.BoolFlag{Name: "json", Usage: "print raw json response"},
	},
	Action: func(c *cli.Context) error {
		sandboxID := strings.TrimSpace(c.String("sandbox-id"))
		if sandboxID == "" {
			return errors.New("sandbox-id is required")
		}
		requestID := uuid.NewString()
		req := &snapshotCreateRequest{
			RequestID:   requestID,
			SandboxID:   sandboxID,
			DisplayName: c.String("display-name"),
		}
		body, err := jsoniter.Marshal(req)
		if err != nil {
			return err
		}
		rsp := &snapshotResponse{}
		if err := doSnapshotReq(c, http.MethodPost, "/cube/snapshot", requestID, bytes.NewBuffer(body), rsp); err != nil {
			return err
		}
		if err := ensureSuccessRet(rsp.Ret); err != nil {
			return err
		}
		if c.Bool("json") {
			commands.PrintAsJSON(rsp)
			return nil
		}
		printSnapshotResponse(rsp)
		return nil
	},
}

var SnapshotListCommand = cli.Command{
	Name:    "list",
	Aliases: []string{"ls"},
	Usage:   "list runtime snapshots",
	Flags: []cli.Flag{
		cli.BoolFlag{Name: "json", Usage: "print raw json response"},
		cli.StringFlag{Name: "output,o", Usage: "output format, set to wide for more columns"},
	},
	Action: func(c *cli.Context) error {
		rsp := &snapshotListResponse{}
		requestID := uuid.NewString()
		if err := doSnapshotReq(c, http.MethodGet, "/cube/snapshot", requestID, nil, rsp); err != nil {
			return err
		}
		if err := ensureSuccessRet(rsp.Ret); err != nil {
			return err
		}
		if c.Bool("json") {
			commands.PrintAsJSON(rsp)
			return nil
		}
		wideOutput := strings.EqualFold(strings.TrimSpace(c.String("output")), "wide")
		w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
		header := "SNAPSHOT_ID\tSTATUS\tSANDBOX_ID\tNODE_ID\tCREATED_AT"
		if wideOutput {
			header = "SNAPSHOT_ID\tSTATUS\tDISPLAY_NAME\tSANDBOX_ID\tNODE_ID\tRUNTIME_REFS\tBACKEND\tLAST_ERROR"
		}
		fmt.Fprintln(w, header)
		for _, item := range rsp.Data {
			originNode := item.OriginNodeID
			if wideOutput {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
					item.SnapshotID, item.Status, item.DisplayName, item.OriginSandboxID, originNode, item.RuntimeRefCount, item.StorageBackend, item.LastError)
				continue
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				item.SnapshotID, item.Status, item.OriginSandboxID, originNode, item.CreatedAt)
		}
		return w.Flush()
	},
}

var SnapshotInfoCommand = cli.Command{
	Name:    "info",
	Aliases: []string{"describe"},
	Usage:   "show snapshot metadata and node replicas",
	Flags: []cli.Flag{
		cli.StringFlag{Name: "snapshot-id", Usage: "snapshot id to query"},
		cli.BoolFlag{Name: "include-request", Usage: "include stored create request"},
		cli.BoolFlag{Name: "json", Usage: "print raw json response"},
	},
	Action: func(c *cli.Context) error {
		snapshotID := c.String("snapshot-id")
		if snapshotID == "" {
			return errors.New("snapshot-id is required")
		}
		requestID := uuid.NewString()
		endpoint := fmt.Sprintf("/cube/snapshot/%s", snapshotID)
		if c.Bool("include-request") {
			endpoint += "?include_request=true"
		}
		rsp := &snapshotResponse{}
		if err := doSnapshotReq(c, http.MethodGet, endpoint, requestID, nil, rsp); err != nil {
			return err
		}
		if err := ensureSuccessRet(rsp.Ret); err != nil {
			return err
		}
		if c.Bool("json") {
			commands.PrintAsJSON(rsp)
			return nil
		}
		printSnapshotResponse(rsp)
		return nil
	},
}

var SnapshotDeleteCommand = cli.Command{
	Name:  "delete",
	Usage: "delete snapshot metadata and node replicas",
	Flags: []cli.Flag{
		cli.StringFlag{Name: "snapshot-id", Usage: "snapshot id to delete"},
		cli.StringFlag{Name: "instance-type", Usage: "instance type override"},
		cli.BoolFlag{Name: "json", Usage: "print raw json response"},
	},
	Action: func(c *cli.Context) error {
		snapshotID := c.String("snapshot-id")
		if snapshotID == "" {
			return errors.New("snapshot-id is required")
		}
		requestID := uuid.NewString()
		query := fmt.Sprintf("?request_id=%s", requestID)
		if instanceType := strings.TrimSpace(c.String("instance-type")); instanceType != "" {
			query += "&instance_type=" + instanceType
		}
		rsp := &operationResponse{}
		if err := doSnapshotReq(c, http.MethodDelete, "/cube/snapshot/"+snapshotID+query, requestID, nil, rsp); err != nil {
			return err
		}
		if err := ensureSuccessRet(rsp.Ret); err != nil {
			return err
		}
		if c.Bool("json") {
			commands.PrintAsJSON(rsp)
			return nil
		}
		printOperationResponse(rsp.Operation)
		return nil
	},
}

var StorageCommand = cli.Command{
	Name:  "storage",
	Usage: "inspect snapshot storage state",
	Subcommands: cli.Commands{
		StorageStatusCommand,
	},
}

var StorageStatusCommand = cli.Command{
	Name:  "status",
	Usage: "show aggregated snapshot storage status from cubemaster",
	Flags: []cli.Flag{
		cli.BoolFlag{Name: "refresh", Usage: "refresh node metrics before returning"},
		cli.BoolFlag{Name: "json", Usage: "print raw json response"},
	},
	Action: func(c *cli.Context) error {
		requestID := uuid.NewString()
		endpoint := "/cube/snapshot/storage"
		if c.Bool("refresh") {
			endpoint += "?refresh=true"
		}
		rsp := &snapshotStorageStatusResponse{}
		if err := doSnapshotReq(c, http.MethodGet, endpoint, requestID, nil, rsp); err != nil {
			return err
		}
		if err := ensureSuccessRet(rsp.Ret); err != nil {
			return err
		}
		if c.Bool("json") {
			commands.PrintAsJSON(rsp)
			return nil
		}
		printSnapshotStorageStatus(rsp.Data)
		return nil
	},
}

var RollbackCommand = cli.Command{
	Name:    "rollback",
	Aliases: []string{"sandbox-rollback"},
	Usage:   "rollback a sandbox to a snapshot",
	Flags: []cli.Flag{
		cli.StringFlag{Name: "sandbox-id", Usage: "sandbox id to rollback"},
		cli.StringFlag{Name: "snapshot-id", Usage: "snapshot id to rollback to"},
		cli.StringFlag{Name: "instance-type", Usage: "instance type override"},
		cli.BoolFlag{Name: "json", Usage: "print raw json response"},
	},
	Action: func(c *cli.Context) error {
		sandboxID := c.String("sandbox-id")
		snapshotID := c.String("snapshot-id")
		if sandboxID == "" || snapshotID == "" {
			return errors.New("sandbox-id and snapshot-id are required")
		}
		requestID := uuid.NewString()
		req := &rollbackRequest{
			RequestID:    requestID,
			SandboxID:    sandboxID,
			SnapshotID:   snapshotID,
			InstanceType: c.String("instance-type"),
		}
		body, err := jsoniter.Marshal(req)
		if err != nil {
			return err
		}
		rsp := &operationResponse{}
		if err := doSnapshotReq(c, http.MethodPost, "/cube/sandbox/rollback", requestID, bytes.NewBuffer(body), rsp); err != nil {
			return err
		}
		if err := ensureSuccessRet(rsp.Ret); err != nil {
			return err
		}
		if c.Bool("json") {
			commands.PrintAsJSON(rsp)
			return nil
		}
		printOperationResponse(rsp.Operation)
		return nil
	},
}

var OperationCommand = cli.Command{
	Name:  "operation",
	Usage: "inspect snapshot operations",
	Subcommands: cli.Commands{
		OperationStatusCommand,
		OperationWatchCommand,
	},
}

var OperationStatusCommand = cli.Command{
	Name:  "status",
	Usage: "show snapshot operation status",
	Flags: []cli.Flag{
		cli.StringFlag{Name: "operation-id", Usage: "snapshot operation id"},
		cli.BoolFlag{Name: "json", Usage: "print raw json response"},
	},
	Action: func(c *cli.Context) error {
		operationID := c.String("operation-id")
		if operationID == "" {
			return errors.New("operation-id is required")
		}
		rsp, err := fetchOperationStatus(c, operationID)
		if err != nil {
			return err
		}
		if c.Bool("json") {
			commands.PrintAsJSON(rsp)
			return nil
		}
		printOperationResponse(rsp.Operation)
		return nil
	},
}

var OperationWatchCommand = cli.Command{
	Name:  "watch",
	Usage: "watch snapshot operation until completion",
	Flags: []cli.Flag{
		cli.StringFlag{Name: "operation-id", Usage: "snapshot operation id"},
		cli.DurationFlag{Name: "interval", Value: 2 * time.Second, Usage: "poll interval"},
		cli.BoolFlag{Name: "json", Usage: "print final raw json response"},
	},
	Action: func(c *cli.Context) error {
		operationID := c.String("operation-id")
		if operationID == "" {
			return errors.New("operation-id is required")
		}
		var lastPrinted string
		for {
			rsp, err := fetchOperationStatus(c, operationID)
			if err != nil {
				return err
			}
			current := ""
			if rsp.Operation != nil {
				current = fmt.Sprintf("%s/%d/%s", rsp.Operation.Status, rsp.Operation.Progress, rsp.Operation.Phase)
			}
			if current != lastPrinted {
				printOperationResponse(rsp.Operation)
				lastPrinted = current
			}
			if operationFinished(rsp.Operation) {
				if c.Bool("json") {
					commands.PrintAsJSON(rsp)
				}
				if rsp.Operation.Status == "FAILED" {
					return errors.New(rsp.Operation.ErrorMessage)
				}
				return nil
			}
			time.Sleep(c.Duration("interval"))
		}
	},
}

func doSnapshotReq(c *cli.Context, method, endpoint, requestID string, body io.Reader, rsp interface{}) error {
	serverList = getServerAddrs(c)
	if len(serverList) == 0 {
		return errors.New("no server addr")
	}
	port = c.GlobalString("port")
	host := serverList[rand.Int()%len(serverList)]
	url := fmt.Sprintf("http://%s%s", net.JoinHostPort(host, port), endpoint)
	return doHttpReq(c, url, method, requestID, body, rsp)
}

func fetchOperationStatus(c *cli.Context, operationID string) (*operationResponse, error) {
	requestID := uuid.NewString()
	rsp := &operationResponse{}
	if err := doSnapshotReq(c, http.MethodGet, "/cube/operation/"+operationID, requestID, nil, rsp); err != nil {
		return nil, err
	}
	if err := ensureSuccessRet(rsp.Ret); err != nil {
		return nil, err
	}
	return rsp, nil
}

func ensureSuccessRet(ret *types.Ret) error {
	if ret == nil {
		return errors.New("empty response")
	}
	if ret.RetCode != 200 {
		return errors.New(ret.RetMsg)
	}
	return nil
}

func operationFinished(info *operationResource) bool {
	return info != nil && (info.Status == "READY" || info.Status == "FAILED")
}

func printSnapshotResponse(rsp *snapshotResponse) {
	if rsp == nil || rsp.Snapshot == nil {
		return
	}
	log.Printf("snapshot_id: %s\n", rsp.Snapshot.SnapshotID)
	log.Printf("status: %s\n", rsp.Snapshot.Status)
	log.Printf("display_name: %s\n", rsp.Snapshot.DisplayName)
	log.Printf("origin_sandbox_id: %s\n", rsp.Snapshot.OriginSandboxID)
	log.Printf("origin_node_id: %s\n", rsp.Snapshot.OriginNodeID)
	log.Printf("storage_backend: %s\n", rsp.Snapshot.StorageBackend)
	log.Printf("runtime_ref_count: %d\n", rsp.Snapshot.RuntimeRefCount)
	if len(rsp.Snapshot.RuntimeRefSandboxes) > 0 {
		log.Printf("runtime_ref_sandboxes: %s\n", strings.Join(rsp.Snapshot.RuntimeRefSandboxes, ","))
	}
	if rsp.Snapshot.LastError != "" {
		log.Printf("last_error: %s\n", rsp.Snapshot.LastError)
	}
	if rsp.Operation != nil {
		printOperationResponse(rsp.Operation)
	}
	w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
	fmt.Fprintln(w, "NODE_ID\tNODE_IP\tSTATUS\tPHASE\tSPEC\tERROR")
	for _, replica := range rsp.Snapshot.Replicas {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			replica.NodeID, replica.NodeIP, replica.Status, replica.Phase, replica.Spec, replica.ErrorMessage)
	}
	_ = w.Flush()
}

func printOperationResponse(info *operationResource) {
	if info == nil {
		return
	}
	log.Printf("operation_id: %s\n", info.OperationID)
	log.Printf("snapshot_id: %s\n", info.SnapshotID)
	log.Printf("sandbox_id: %s\n", info.SandboxID)
	log.Printf("operation: %s\n", info.Operation)
	log.Printf("status: %s\n", info.Status)
	log.Printf("phase: %s\n", info.Phase)
	log.Printf("progress: %d%%\n", info.Progress)
	if info.ErrorMessage != "" {
		log.Printf("error_message: %s\n", info.ErrorMessage)
	}
}

func printSnapshotStorageStatus(items []*snapshotStorageStatus) {
	w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
	fmt.Fprintln(w, "NODE_ID\tNODE_IP\tMODE\tUSAGE_PCT\tLAST_ERROR\tUPDATED_AT")
	for _, item := range items {
		if item == nil {
			continue
		}
		updatedAt := ""
		if item.LastUpdatedAt > 0 {
			updatedAt = time.Unix(item.LastUpdatedAt, 0).Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
			item.NodeID, item.NodeIP, item.Mode, item.UsagePct, item.LastError, updatedAt)
	}
	_ = w.Flush()
}

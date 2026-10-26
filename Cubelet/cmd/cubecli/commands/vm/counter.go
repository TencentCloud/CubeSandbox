// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package vm

import (
	gocontext "context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"

	"github.com/containerd/errdefs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	"github.com/urfave/cli/v2"
)

type Counters struct {
	Blk0 struct {
		WriteBytes   int `json:"write_bytes"`
		WriteOps     int `json:"write_ops"`
		ReadBytes    int `json:"read_bytes"`
		LimitByBytes int `json:"limit_by_bytes"`
		LimitByOps   int `json:"limit_by_ops"`
		ReadOps      int `json:"read_ops"`
	} `json:"blk-0"`
	Tap0 struct {
		TxLimitFrames int `json:"tx_limit_frames"`
		RxBytes       int `json:"rx_bytes"`
		RxLimitBytes  int `json:"rx_limit_bytes"`
		RxLimitFrames int `json:"rx_limit_frames"`
		TxFrames      int `json:"tx_frames"`
		TxBytes       int `json:"tx_bytes"`
		TxLimitBytes  int `json:"tx_limit_bytes"`
		RxFrames      int `json:"rx_frames"`
	} `json:"tap-0"`
	CubeFs struct {
		TotalBytes   int `json:"total_bytes"`
		LimitByBytes int `json:"limit_by_bytes"`
		TotalOps     int `json:"total_ops"`
		LimitByOps   int `json:"limit_by_ops"`
	} `json:"cube-fs"`
}

var CounterCommand = &cli.Command{
	Name:                   "counter",
	Usage:                  "counter [OPTIONS] CONTAINER",
	UsageText:              "inspect vm level counters",
	UseShortOptionHandling: true,
	ArgsUsage:              "[flags] CONTAINER",
	Flags:                  []cli.Flag{},
	Action: func(context *cli.Context) error {
		id := context.Args().First()
		if id == "" {
			return fmt.Errorf("container id must be provided: %w", errdefs.ErrInvalidArgument)
		}

		cntdClient, cntCtx, err := commands.NewDefaultContainerdClient(context)
		if err != nil {
			return fmt.Errorf("init containerd connect failed.%s", err)
		}
		defer cntdClient.Close()

		longId, err := commands.CompleteShortId(cntCtx, cntdClient, id)
		if err == nil {
			id = longId
		}

		socketPath := filepath.Join("/run/vc/vm", id, "cube-api.sock")
		unixURL := &url.URL{
			Scheme: "http",
			Host:   "localhost",
			Path:   "/api/v1/vm.counters",
		}
		unixTransport := &http.Transport{
			DialContext: func(ctx gocontext.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		}
		unixClient := &http.Client{
			Transport: unixTransport,
		}
		req, err := http.NewRequest("GET", unixURL.String(), nil)
		if err != nil {
			return err
		}
		resp, err := unixClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		var counter Counters
		err = json.NewDecoder(resp.Body).Decode(&counter)
		if err != nil {
			return err
		}

		fmt.Printf("blk-0:\n")
		fmt.Printf("  write_bytes: %d\n", counter.Blk0.WriteBytes)
		fmt.Printf("  write_ops: %d\n", counter.Blk0.WriteOps)
		fmt.Printf("  read_bytes: %d\n", counter.Blk0.ReadBytes)
		fmt.Printf("  limit_by_bytes: %d\n", counter.Blk0.LimitByBytes)
		fmt.Printf("  limit_by_ops: %d\n", counter.Blk0.LimitByOps)
		fmt.Printf("  read_ops: %d\n", counter.Blk0.ReadOps)
		fmt.Printf("tap-0:\n")
		fmt.Printf("  tx_limit_frames: %d\n", counter.Tap0.TxLimitFrames)
		fmt.Printf("  rx_bytes: %d\n", counter.Tap0.RxBytes)
		fmt.Printf("  rx_limit_bytes: %d\n", counter.Tap0.RxLimitBytes)
		fmt.Printf("  rx_limit_frames: %d\n", counter.Tap0.RxLimitFrames)
		fmt.Printf("  tx_frames: %d\n", counter.Tap0.TxFrames)
		fmt.Printf("  tx_bytes: %d\n", counter.Tap0.TxBytes)
		fmt.Printf("  tx_limit_bytes: %d\n", counter.Tap0.TxLimitBytes)
		fmt.Printf("  rx_frames: %d\n", counter.Tap0.RxFrames)
		fmt.Printf("cube-fs:\n")
		fmt.Printf("  total_bytes: %d\n", counter.CubeFs.TotalBytes)
		fmt.Printf("  limit_by_bytes: %d\n", counter.CubeFs.LimitByBytes)
		fmt.Printf("  total_ops: %d\n", counter.CubeFs.TotalOps)
		fmt.Printf("  limit_by_ops: %d\n", counter.CubeFs.LimitByOps)

		return nil
	},
}

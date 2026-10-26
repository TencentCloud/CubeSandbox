// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package container

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/cmd/cubecli/commands"
	networkruntime "github.com/tencentcloud/CubeSandbox/Cubelet/network/runtime"
	"github.com/urfave/cli/v2"
)

const defaultCubeletHTTPAddress = "http://127.0.0.1:9998"

// ListTapCommand reads TAP state from Cubelet's embedded network runtime instead of
// talking to the removed standalone network-agent process.
var ListTapCommand = &cli.Command{
	Name:  "taps",
	Usage: "list network runtime tap states from cubelet",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "http-address",
			Usage:   "cubelet HTTP server address for network runtime diagnostics",
			EnvVars: []string{"CUBELET_HTTP_ADDRESS"},
			Value:   defaultCubeletHTTPAddress,
		},
		&cli.BoolFlag{
			Name:  "json",
			Usage: "output raw JSON response",
		},
	},
	Action: func(clictx *cli.Context) error {
		resp, err := fetchNetworkRuntimeTaps(clictx.String("http-address"), clictx.Duration("timeout"))
		if err != nil {
			return err
		}
		if clictx.Bool("json") {
			commands.PrintAsJSON(resp)
			return nil
		}
		return printNetworkRuntimeTaps(resp.Taps)
	},
}

// fetchNetworkRuntimeTaps performs the small diagnostic HTTP call used by the CLI.
// Keeping this path HTTP-only lets operators inspect runtime state without reviving
// the old network-agent gRPC client stack.
func fetchNetworkRuntimeTaps(address string, timeout time.Duration) (*networkruntime.ListTapsResponse, error) {
	endpoint, err := networkRuntimeTapsURL(address)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cubelet network taps request failed: status=%s", resp.Status)
	}
	var out networkruntime.ListTapsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode cubelet network taps response: %w", err)
	}
	return &out, nil
}

// networkRuntimeTapsURL accepts either a bare host:port or a full base URL, then
// normalizes it to the fixed Cubelet diagnostics endpoint.
func networkRuntimeTapsURL(address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		address = defaultCubeletHTTPAddress
	}
	if !strings.Contains(address, "://") {
		address = "http://" + address
	}
	u, err := url.Parse(address)
	if err != nil {
		return "", fmt.Errorf("parse cubelet HTTP address %q: %w", address, err)
	}
	u.Path = "/v1/network/taps"
	u.RawQuery = ""
	return u.String(), nil
}

// printNetworkRuntimeTaps renders the human-oriented view. The JSON flag keeps
// the complete runtime response available for scripts and deeper debugging.
func printNetworkRuntimeTaps(taps []networkruntime.TapState) error {
	display := commands.NewDefaultTableDisplay()
	display.AddRow([]string{"STATE", "SANDBOX", "OWNER", "TAP", "IFINDEX", "SANDBOX-IP", "RETRIES", "LAST-ERROR", "PORTS"})
	for _, state := range taps {
		display.AddRow([]string{
			state.State,
			state.SandboxID,
			state.OwnerSandboxID,
			state.TapName,
			fmt.Sprintf("%d", state.TapIfIndex),
			state.SandboxIP,
			fmt.Sprintf("%d", state.RetryCount),
			formatNetworkRuntimeLastError(state.LastError),
			formatNetworkRuntimePorts(state.PortMappings),
		})
	}
	return display.Flush()
}

func formatNetworkRuntimeLastError(lastError string) string {
	lastError = strings.TrimSpace(lastError)
	if lastError == "" {
		return "-"
	}
	return lastError
}

func formatNetworkRuntimePorts(mappings []networkruntime.PortMapping) string {
	if len(mappings) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(mappings))
	for _, pm := range mappings {
		protocol := pm.Protocol
		if protocol == "" {
			protocol = "tcp"
		}
		hostIP := pm.HostIP
		if hostIP == "" {
			hostIP = "127.0.0.1"
		}
		parts = append(parts, fmt.Sprintf("%s:%d->%d/%s", hostIP, pm.HostPort, pm.ContainerPort, protocol))
	}
	return strings.Join(parts, ",")
}

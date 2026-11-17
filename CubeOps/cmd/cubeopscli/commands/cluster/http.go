// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/urfave/cli"
)

// buildURL picks one of the comma-separated CubeOps addresses.
func buildURL(c *cli.Context, path string) string {
	addrs := strings.Split(c.GlobalString("address"), ",")
	host := strings.TrimSpace(addrs[rand.Intn(len(addrs))])
	return fmt.Sprintf("http://%s%s", net.JoinHostPort(strings.Trim(host, "[]"), c.GlobalString("port")), path)
}

func doReq(c *cli.Context, url, method string, body []byte, rsp interface{}) error {
	timeout := c.GlobalDuration("timeout")
	if timeout <= 0 {
		timeout = 35 * time.Second
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, string(data))
	}
	if rsp != nil && len(data) > 0 {
		return json.Unmarshal(data, rsp)
	}
	return nil
}

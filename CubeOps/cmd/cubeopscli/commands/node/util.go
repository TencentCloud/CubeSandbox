// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package node

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

var (
	serverList []string
	port       string
	timeout    time.Duration
)

func getServerAddrs(c *cli.Context) []string {
	addrs := c.GlobalString("address")
	if addrs == "" {
		return nil
	}
	parts := strings.Split(addrs, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func doHttpReq(c *cli.Context, url, method, requestID string, body io.Reader, rsp interface{}) error {
	port = c.GlobalString("port")
	timeout = c.GlobalDuration("timeout")
	if timeout <= 0 {
		timeout = 35 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}
	if requestID != "" {
		req.Header.Set("X-Request-ID", requestID)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
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

func pickHost() (string, error) {
	if len(serverList) == 0 {
		return "", fmt.Errorf("no server addr")
	}
	return serverList[rand.Int()%len(serverList)], nil
}

func buildURL(host, path string) string {
	return fmt.Sprintf("http://%s%s", net.JoinHostPort(strings.Trim(host, "[]"), port), path)
}

func marshalBody(v interface{}) io.Reader {
	if v == nil {
		return bytes.NewReader(nil)
	}
	data, _ := json.Marshal(v)
	return bytes.NewReader(data)
}

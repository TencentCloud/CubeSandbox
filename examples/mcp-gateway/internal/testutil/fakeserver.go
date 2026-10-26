// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides a stdio MCP server that tests start by
// re-executing the test binary.
package testutil

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// FakeServerEnv switches a re-executed test binary into fake-server mode.
const FakeServerEnv = "CUBE_MCP_FAKE_SERVER"

// FakeNameEnv names the fake server in tool output.
const FakeNameEnv = "CUBE_MCP_FAKE_NAME"

type echoArgs struct {
	Text string `json:"text"`
}

// MaybeRunFakeServer serves the fake MCP server over stdio and exits when
// FakeServerEnv is set. Call it first in TestMain.
func MaybeRunFakeServer() {
	if os.Getenv(FakeServerEnv) != "1" {
		return
	}
	name := os.Getenv(FakeNameEnv)
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-" + name, Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "echo text"},
		func(_ context.Context, _ *mcp.CallToolRequest, in echoArgs) (*mcp.CallToolResult, any, error) {
			return text(name + ":" + in.Text), nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: name + "_env", Description: "report environment"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			_, hasToken := os.LookupEnv("GATEWAY_ACCESS_TOKEN")
			return text(fmt.Sprintf("secret=%s token=%t", os.Getenv("FAKE_SECRET"), hasToken)), nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: name + "_crash", Description: "exit the process"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			os.Exit(3)
			return nil, nil, nil
		})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "fake server:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// FakeRunCmd returns a shell command that starts the fake server named name.
func FakeRunCmd(name string) string {
	return fmt.Sprintf("%s=1 %s=%s exec %s -test.run=^$", FakeServerEnv, FakeNameEnv, name, os.Args[0])
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// Connect opens a streamable-HTTP MCP client session with a bearer token.
func Connect(t *testing.T, endpoint, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: bearerTransport{token: token, base: http.DefaultTransport}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", endpoint, err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// CallText calls a tool and returns its concatenated text content.
func CallText(t *testing.T, session *mcp.ClientSession, name string, args any) string {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "")
}

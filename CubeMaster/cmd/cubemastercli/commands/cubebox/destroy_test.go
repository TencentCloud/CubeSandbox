package cubebox

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

func newDestroyApp() *cli.App {
	app := cli.NewApp()
	app.Commands = []cli.Command{DestroyCommand}
	app.Flags = []cli.Flag{
		cli.StringFlag{Name: "address, a", Value: "0.0.0.0"},
		cli.StringFlag{Name: "port, p", Value: "8089"},
		cli.DurationFlag{Name: "timeout", Value: 35 * time.Second},
	}
	return app
}

func newDestroyContext(t *testing.T, globalArgs []string, args []string) *cli.Context {
	t.Helper()

	app := newDestroyApp()

	globalSet := flag.NewFlagSet("global", flag.ContinueOnError)
	for _, cliFlag := range []cli.Flag{
		cli.StringFlag{Name: "address, a", Value: "0.0.0.0"},
		cli.StringFlag{Name: "port, p", Value: "8089"},
		cli.DurationFlag{Name: "timeout", Value: 35 * time.Second},
	} {
		cliFlag.Apply(globalSet)
	}
	if err := globalSet.Parse(globalArgs); err != nil {
		t.Fatalf("parse global args %v: %v", globalArgs, err)
	}
	parent := cli.NewContext(app, globalSet, nil)

	set := flag.NewFlagSet("destroy", flag.ContinueOnError)
	for _, cliFlag := range DestroyCommand.Flags {
		cliFlag.Apply(set)
	}
	if err := set.Parse(args); err != nil {
		t.Fatalf("parse destroy args %v: %v", args, err)
	}

	ctx := cli.NewContext(app, set, parent)
	ctx.Command = DestroyCommand
	return ctx
}

func splitHostPort(t *testing.T, serverURL string) (string, string) {
	t.Helper()

	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse server url %q: %v", serverURL, err)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split host/port from %q: %v", u.Host, err)
	}
	return host, port
}

func TestDestroyCommandNoArgsReturnsError(t *testing.T) {
	ctx := newDestroyContext(t, nil, nil)
	action, ok := DestroyCommand.Action.(func(*cli.Context) error)
	if !ok {
		t.Fatal("destroy action type assertion failed")
	}
	helpBuffer := &bytes.Buffer{}
	ctx.App.Writer = helpBuffer

	err := action(ctx)
	if err == nil {
		t.Fatal("expected error when no sandbox id is provided")
	}
	output := helpBuffer.String()
	if !strings.Contains(output, "destroy sandbox instances") {
		t.Fatalf("output=%q, missing usage description", output)
	}
}

func TestDestroyCommandAliasRmWorks(t *testing.T) {
	deleteCnt := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method=%s, want DELETE", r.Method)
		}
		if r.URL.Path != "/cube/sandbox" {
			t.Fatalf("path=%s, want /cube/sandbox", r.URL.Path)
		}
		deleteCnt++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"requestID":"req-rm","ret":{"ret_code":200,"ret_msg":"OK"}}`)
	}))
	defer server.Close()

	host, serverPort := splitHostPort(t, server.URL)
	app := newDestroyApp()

	err := app.Run([]string{"cubemastercli", "--address", host, "--port", serverPort, "--timeout", "1s", "rm", "sb-rm"})
	if err != nil {
		t.Fatalf("run rm alias error=%v", err)
	}
	if deleteCnt != 1 {
		t.Fatalf("delete request count=%d, want 1", deleteCnt)
	}
}

func TestDestroyCommandSuccessWithMultipleSandboxes(t *testing.T) {
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method=%s, want DELETE", r.Method)
		}
		if r.URL.Path != "/cube/sandbox" {
			t.Fatalf("path=%s, want /cube/sandbox", r.URL.Path)
		}
		req := &types.DeleteCubeSandboxReq{}
		if err := json.NewDecoder(r.Body).Decode(req); err != nil {
			t.Fatalf("decode request body error=%v", err)
		}
		seen[req.SandboxID] = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"requestID":"req-ok","ret":{"ret_code":200,"ret_msg":"OK"}}`)
	}))
	defer server.Close()

	host, serverPort := splitHostPort(t, server.URL)
	ctx := newDestroyContext(
		t,
		[]string{"--address", host, "--port", serverPort, "--timeout", "1s"},
		[]string{"sb-1", "sb-2"},
	)
	action, ok := DestroyCommand.Action.(func(*cli.Context) error)
	if !ok {
		t.Fatal("destroy action type assertion failed")
	}

	err := action(ctx)
	if err != nil {
		t.Fatalf("destroy action error=%v", err)
	}
	if !seen["sb-1"] || !seen["sb-2"] || len(seen) != 2 {
		t.Fatalf("seen sandbox ids=%v, want sb-1 and sb-2", seen)
	}
}

func TestDestroyCommandPartialFailureReturnsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method=%s, want DELETE", r.Method)
		}
		req := &types.DeleteCubeSandboxReq{}
		if err := json.NewDecoder(r.Body).Decode(req); err != nil {
			t.Fatalf("decode request body error=%v", err)
		}
		if !req.Sync {
			t.Fatalf("sync=%v, want true", req.Sync)
		}
		w.Header().Set("Content-Type", "application/json")
		if req.SandboxID == "sb-fail" {
			_, _ = io.WriteString(w, `{"requestID":"req-fail","ret":{"ret_code":500,"ret_msg":"mock destroy failed"}}`)
			return
		}
		_, _ = io.WriteString(w, fmt.Sprintf(`{"requestID":"req-%s","ret":{"ret_code":200,"ret_msg":"OK"}}`, req.SandboxID))
	}))
	defer server.Close()

	host, serverPort := splitHostPort(t, server.URL)
	ctx := newDestroyContext(
		t,
		[]string{"--address", host, "--port", serverPort, "--timeout", "1s"},
		[]string{"sb-ok", "sb-fail"},
	)
	action, ok := DestroyCommand.Action.(func(*cli.Context) error)
	if !ok {
		t.Fatal("destroy action type assertion failed")
	}

	actionErr := action(ctx)
	if actionErr == nil {
		t.Fatal("expected error for partial failure")
	}
	errText := actionErr.Error()
	if !strings.Contains(errText, "sb-fail") {
		t.Fatalf("error=%q, missing sandbox id", errText)
	}
	if !strings.Contains(errText, "mock destroy failed") {
		t.Fatalf("error=%q, missing backend error message", errText)
	}
}

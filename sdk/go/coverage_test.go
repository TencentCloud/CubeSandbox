// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWithHTTPClientOption(t *testing.T) {
	custom := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"status":"ok"}`)),
			Request:    req,
		}, nil
	})}

	client := NewClient(Config{}, WithHTTPClient(nil), WithHTTPClient(custom))
	if client.controlHTTP != custom || client.dataHTTP != custom {
		t.Fatalf("custom client was not installed")
	}

	health, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if health["status"] != "ok" {
		t.Fatalf("health=%#v", health)
	}
}

func TestClientClose(t *testing.T) {
	var nilClient *Client
	if err := nilClient.Close(); err != nil {
		t.Fatalf("nil Client.Close returned error: %v", err)
	}

	controlTransport := &closeCountingTransport{}
	dataTransport := &closeCountingTransport{}
	client := &Client{
		controlHTTP: &http.Client{Transport: controlTransport},
		dataHTTP:    &http.Client{Transport: dataTransport},
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Client.Close returned error: %v", err)
	}
	if controlTransport.closes != 1 || dataTransport.closes != 1 {
		t.Fatalf("close count mismatch: control=%d data=%d", controlTransport.closes, dataTransport.closes)
	}

	sharedTransport := &closeCountingTransport{}
	sharedHTTP := &http.Client{Transport: sharedTransport}
	client = NewClient(Config{}, WithHTTPClient(sharedHTTP))
	if err := client.Close(); err != nil {
		t.Fatalf("Client.Close with shared HTTP client returned error: %v", err)
	}
	if sharedTransport.closes != 1 {
		t.Fatalf("shared HTTP client closed %d times, want 1", sharedTransport.closes)
	}

	if err := NewClient(Config{}).Close(); err != nil {
		t.Fatalf("default Client.Close returned error: %v", err)
	}
}

func TestClientRequestErrorPaths(t *testing.T) {
	client := NewClient(Config{APIURL: "http://127.0.0.1:1"})

	if err := client.doJSON(context.Background(), http.MethodPost, "/x", map[string]any{"bad": func() {}}, nil, http.StatusOK); err == nil {
		t.Fatal("doJSON with unmarshalable body returned nil error")
	}

	client.config.APIURL = "http://%"
	if err := client.doJSON(context.Background(), http.MethodGet, "/x", nil, nil, http.StatusOK); err == nil {
		t.Fatal("doJSON with malformed URL returned nil error")
	}

	boom := errors.New("boom")
	client = NewClient(Config{}, WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, boom
	})}))
	if err := client.doJSON(context.Background(), http.MethodGet, "/x", nil, nil, http.StatusOK); !errors.Is(err, boom) {
		t.Fatalf("doJSON transport error=%v, want %v", err, boom)
	}

	client = NewClient(Config{}, WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("{")),
			Request:    req,
		}, nil
	})}))
	var out map[string]any
	if err := client.doJSON(context.Background(), http.MethodGet, "/x", nil, &out, http.StatusOK); err == nil {
		t.Fatal("doJSON with malformed JSON response returned nil error")
	}
}

func TestListErrorPathsAndAttachDomain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/sandboxes":
			http.Error(w, `{"message":"list failed"}`, http.StatusInternalServerError)
		case "/v2/sandboxes":
			http.Error(w, `{"message":"list v2 failed"}`, http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(Config{APIURL: server.URL, SandboxDomain: "domain.test"})
	if _, err := client.List(context.Background()); err == nil {
		t.Fatal("List returned nil error")
	}
	if _, err := client.ListV2(context.Background()); err == nil {
		t.Fatal("ListV2 returned nil error")
	}

	sb := Sandbox{SandboxID: "sb-domain"}
	client.attachSandbox(&sb)
	if sb.Domain != "domain.test" {
		t.Fatalf("Domain=%q", sb.Domain)
	}
	sb.Domain = "response.domain"
	client.attachSandbox(&sb)
	if sb.Domain != "response.domain" {
		t.Fatalf("Domain was overwritten: %q", sb.Domain)
	}
}

func TestSandboxRequiresAttachedClient(t *testing.T) {
	ctx := context.Background()
	var nilSandbox *Sandbox
	if err := nilSandbox.ensureClient(); err == nil {
		t.Fatal("nil sandbox ensureClient returned nil error")
	}
	if _, err := (&Sandbox{}).GetInfo(ctx); err == nil {
		t.Fatal("GetInfo without client returned nil error")
	}
	if err := (&Sandbox{}).Pause(ctx, PauseOptions{}); err == nil {
		t.Fatal("Pause without client returned nil error")
	}
	if err := (&Sandbox{}).Resume(ctx, nil); err == nil {
		t.Fatal("Resume without client returned nil error")
	}
	if err := (&Sandbox{}).Kill(ctx); err == nil {
		t.Fatal("Kill without client returned nil error")
	}
	if _, err := (&Sandbox{}).RunCode(ctx, "1", RunCodeOptions{}); err == nil {
		t.Fatal("RunCode without client returned nil error")
	}
	if err := (&Sandbox{}).SetTimeout(ctx, time.Second); err == nil {
		t.Fatal("SetTimeout without client returned nil error")
	}
	if err := (*Sandbox)(nil).SetTimeout(ctx, time.Second); err == nil {
		t.Fatal("SetTimeout on nil sandbox returned nil error")
	}
}

func TestSandboxPauseBranches(t *testing.T) {
	t.Run("post error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"sandbox not found"}`, http.StatusNotFound)
		}))
		defer server.Close()

		sb := &Sandbox{client: NewClient(Config{APIURL: server.URL}), SandboxID: "missing"}
		if err := sb.Pause(context.Background(), PauseOptions{}); !errors.Is(err, ErrSandboxNotFound) {
			t.Fatalf("Pause error=%v, want ErrSandboxNotFound", err)
		}
	})

	t.Run("wait success with negative interval", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pause"):
				w.WriteHeader(http.StatusNoContent)
			case r.Method == http.MethodGet:
				fmt.Fprint(w, sandboxInfoJSON("sb-pause", "paused"))
			default:
				t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		}))
		defer server.Close()

		sb := &Sandbox{client: NewClient(Config{APIURL: server.URL}), SandboxID: "sb-pause"}
		if err := sb.Pause(context.Background(), PauseOptions{Timeout: 50 * time.Millisecond, Interval: -1}); err != nil {
			t.Fatalf("Pause returned error: %v", err)
		}
	})

	t.Run("default timeout with already paused sandbox", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			fmt.Fprint(w, sandboxInfoJSON("sb-default-timeout", "paused"))
		}))
		defer server.Close()

		sb := &Sandbox{client: NewClient(Config{APIURL: server.URL}), SandboxID: "sb-default-timeout"}
		if err := sb.Pause(context.Background(), PauseOptions{}); err != nil {
			t.Fatalf("Pause returned error: %v", err)
		}
	})

	t.Run("get info error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.Error(w, `{"message":"gone"}`, http.StatusNotFound)
		}))
		defer server.Close()

		sb := &Sandbox{client: NewClient(Config{APIURL: server.URL}), SandboxID: "sb-gone"}
		if err := sb.Pause(context.Background(), PauseOptions{Timeout: time.Second}); !errors.Is(err, ErrSandboxNotFound) {
			t.Fatalf("Pause error=%v, want ErrSandboxNotFound", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			fmt.Fprint(w, sandboxInfoJSON("sb-timeout", "running"))
		}))
		defer server.Close()

		sb := &Sandbox{client: NewClient(Config{APIURL: server.URL}), SandboxID: "sb-timeout"}
		err := sb.Pause(context.Background(), PauseOptions{Timeout: time.Millisecond, Interval: time.Millisecond})
		if err == nil || !strings.Contains(err.Error(), "did not reach 'paused'") {
			t.Fatalf("Pause timeout error=%v", err)
		}
	})

	t.Run("context cancelled while waiting", func(t *testing.T) {
		ctx, cancelFn := context.WithCancel(context.Background())
		roundTrips := 0
		client := NewClient(Config{}, WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			roundTrips++
			if roundTrips == 1 {
				return &http.Response{
					StatusCode: http.StatusNoContent,
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: cancelOnCloseBody{
					Reader: strings.NewReader(sandboxInfoJSON("sb-cancel", "running")),
					cancel: cancelFn,
				},
				Request: req,
			}, nil
		})}))
		sb := &Sandbox{client: client, SandboxID: "sb-cancel"}
		err := sb.Pause(ctx, PauseOptions{Timeout: time.Second, Interval: 20 * time.Millisecond})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Pause error=%v, want context.Canceled", err)
		}
	})
}

func TestResumeDefaultTimeoutAndErrors(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	sb := &Sandbox{
		client:    NewClient(Config{APIURL: server.URL, Timeout: 123 * time.Second}),
		SandboxID: "sb-resume",
	}
	// Resume now sends exactly the timeout it is given; nil omits the field.
	if err := sb.Resume(context.Background(), DurationPtr(123*time.Second)); err != nil {
		t.Fatalf("Resume returned error: %v", err)
	}
	if !strings.Contains(gotBody, `"timeout":123`) {
		t.Fatalf("resume body=%s", gotBody)
	}

	sb.client.config.APIURL = "http://%"
	if err := sb.Resume(context.Background(), DurationPtr(time.Second)); err == nil {
		t.Fatal("Resume with malformed URL returned nil error")
	}
}

func TestKillErrorPath(t *testing.T) {
	sb := &Sandbox{
		client:    NewClient(Config{APIURL: "http://%"}),
		SandboxID: "sb-kill",
	}
	if err := sb.Kill(context.Background()); err == nil {
		t.Fatal("Kill with malformed URL returned nil error")
	}
}

func TestSandboxAccessors(t *testing.T) {
	if err := (*Sandbox)(nil).Close(); err != nil {
		t.Fatalf("nil Sandbox.Close returned error: %v", err)
	}
	if err := (&Sandbox{}).Close(); err != nil {
		t.Fatalf("detached Sandbox.Close returned error: %v", err)
	}

	transport := &closeCountingTransport{}
	sb := &Sandbox{client: &Client{controlHTTP: &http.Client{Transport: transport}}}
	if err := sb.Close(); err != nil {
		t.Fatalf("Sandbox.Close returned error: %v", err)
	}
	if transport.closes != 1 {
		t.Fatalf("Sandbox.Close closed idle connections %d times, want 1", transport.closes)
	}
	if sb.Commands() == nil {
		t.Fatal("Commands returned nil")
	}
	if sb.Files() == nil {
		t.Fatal("Files returned nil")
	}
}

func TestRunCodeErrorPaths(t *testing.T) {
	t.Run("request build error", func(t *testing.T) {
		sb := &Sandbox{
			client:    NewClient(Config{}),
			SandboxID: "sb-run",
			Domain:    "%",
		}
		if _, err := sb.RunCode(context.Background(), "1", RunCodeOptions{}); err == nil {
			t.Fatal("RunCode with malformed URL returned nil error")
		}
	})

	t.Run("transport error", func(t *testing.T) {
		boom := errors.New("boom")
		client := NewClient(Config{}, WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, boom
		})}))
		sb := &Sandbox{client: client, SandboxID: "sb-run", Domain: "cube.test"}
		if _, err := sb.RunCode(context.Background(), "1", RunCodeOptions{}); !errors.Is(err, boom) {
			t.Fatalf("RunCode transport error=%v", err)
		}
	})

	t.Run("http status error", func(t *testing.T) {
		client := NewClient(Config{}, WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Body:       io.NopCloser(strings.NewReader("bad gateway")),
				Request:    req,
			}, nil
		})}))
		sb := &Sandbox{client: client, SandboxID: "sb-run", Domain: "cube.test"}
		_, err := sb.RunCode(context.Background(), "1", RunCodeOptions{})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadGateway {
			t.Fatalf("RunCode error=%v", err)
		}
	})

	t.Run("scanner error", func(t *testing.T) {
		client := NewClient(Config{}, WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", 17*1024*1024))),
				Request:    req,
			}, nil
		})}))
		sb := &Sandbox{client: client, SandboxID: "sb-run", Domain: "cube.test"}
		if _, err := sb.RunCode(context.Background(), "1", RunCodeOptions{}); err == nil {
			t.Fatal("RunCode with oversized stream line returned nil error")
		}
	})

	t.Run("timeout option", func(t *testing.T) {
		client := NewClient(Config{}, WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(50 * time.Millisecond):
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			}
		})}))
		sb := &Sandbox{client: client, SandboxID: "sb-run", Domain: "cube.test"}
		if _, err := sb.RunCode(context.Background(), "1", RunCodeOptions{Timeout: time.Millisecond}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("RunCode timeout error=%v", err)
		}
	})
}

func TestCommandFallbacksAndErrors(t *testing.T) {
	boom := errors.New("boom")
	_, err := (&Commands{starter: &fakeProcessStarter{err: boom}}).Run(context.Background(), "echo x", CommandOptions{})
	if !errors.Is(err, boom) {
		t.Fatalf("command error=%v", err)
	}

	result, err := (&Commands{starter: &fakeProcessStarter{
		result: &processStartResult{
			Stdout:   "out\n",
			Stderr:   "err\n",
			ExitCode: 1,
		},
	}}).Run(context.Background(), "bad", CommandOptions{})
	if err != nil {
		t.Fatalf("command returned error: %v", err)
	}
	if result.ExitCode != 1 || result.Stdout != "out\n" || result.Stderr != "err\n" {
		t.Fatalf("result=%#v", result)
	}

	result, err = (&Commands{starter: &fakeProcessStarter{
		result: &processStartResult{
			ExitCode: -2,
		},
	}}).Run(context.Background(), "exit -2", CommandOptions{})
	if err != nil {
		t.Fatalf("negative command returned error: %v", err)
	}
	if result.ExitCode != -2 || result.Stdout != "" {
		t.Fatalf("negative result=%#v", result)
	}

	if _, err = (&Commands{}).Run(context.Background(), "true", CommandOptions{}); err == nil || !strings.Contains(err.Error(), "not attached") {
		t.Fatalf("unattached commands error=%v", err)
	}
}

func TestFilesReadErrorAndMainTextFallback(t *testing.T) {
	boom := errors.New("boom")
	if _, err := (&Files{reader: &fakeFileReader{err: boom}}).Read(context.Background(), "/tmp/x"); !errors.Is(err, boom) {
		t.Fatalf("Files.Read error=%v", err)
	}

	content, err := (&Files{reader: &fakeFileReader{content: "main"}}).Read(context.Background(), "/tmp/x")
	if err != nil || content != "main" {
		t.Fatalf("content=%q", content)
	}

	if _, err = (&Files{}).Read(context.Background(), "/tmp/x"); err == nil || !strings.Contains(err.Error(), "not attached") {
		t.Fatalf("unattached files error=%v", err)
	}

	if got := (*Execution)(nil).mainText(); got != "" {
		t.Fatalf("nil execution mainText=%q", got)
	}
	if got := (&Execution{Text: "explicit"}).mainText(); got != "explicit" {
		t.Fatalf("explicit mainText=%q", got)
	}
	if got := (&Execution{}).mainText(); got != "" {
		t.Fatalf("empty mainText=%q", got)
	}
}

func TestConfigParsingEdges(t *testing.T) {
	t.Setenv("CUBE_PROXY_PORT_HTTP", "abc")
	if got := parseIntEnv("CUBE_PROXY_PORT_HTTP", 99); got != 99 {
		t.Fatalf("invalid int=%d", got)
	}
	t.Setenv("CUBE_PROXY_PORT_HTTP", "-1")
	if got := parseIntEnv("CUBE_PROXY_PORT_HTTP", 99); got != 99 {
		t.Fatalf("negative int=%d", got)
	}
	t.Setenv("CUBE_PROXY_PORT_HTTP", "123")
	if got := parseIntEnv("CUBE_PROXY_PORT_HTTP", 99); got != 123 {
		t.Fatalf("parsed int=%d", got)
	}

	if got := normalizeProxyScheme("", 443); got != "https" {
		t.Fatalf("default 443 proxy scheme=%q", got)
	}
	if got := normalizeProxyScheme("HTTPS", 80); got != "https" {
		t.Fatalf("explicit proxy scheme=%q", got)
	}
	if got := normalizeProxyScheme("ftp", 80); got != "http" {
		t.Fatalf("invalid proxy scheme fallback=%q", got)
	}

	t.Setenv("CUBE_TIMEOUT", "-2s")
	if got := parseDurationEnv("CUBE_TIMEOUT", 7*time.Second); got != 7*time.Second {
		t.Fatalf("negative duration=%s", got)
	}
	t.Setenv("CUBE_TIMEOUT", "bad")
	if got := parseDurationEnv("CUBE_TIMEOUT", 7*time.Second); got != 7*time.Second {
		t.Fatalf("bad duration=%s", got)
	}
	t.Setenv("CUBE_TIMEOUT", "1.5")
	if got := parseDurationEnv("CUBE_TIMEOUT", 7*time.Second); got != 1500*time.Millisecond {
		t.Fatalf("float seconds duration=%s", got)
	}

	if got := durationSeconds(0); got != 0 {
		t.Fatalf("durationSeconds(0)=%d", got)
	}
	if got := durationSeconds(1500 * time.Millisecond); got != 2 {
		t.Fatalf("durationSeconds(1.5s)=%d", got)
	}
}

func TestAPIErrorAndMessageEdges(t *testing.T) {
	if got := (*APIError)(nil).Error(); got != "<nil>" {
		t.Fatalf("nil APIError Error=%q", got)
	}
	if got := (&APIError{Message: "plain"}).Error(); got != "plain" {
		t.Fatalf("plain APIError Error=%q", got)
	}
	if got := (&APIError{StatusCode: 418, Message: "teapot"}).Error(); got != "teapot (HTTP 418)" {
		t.Fatalf("status APIError Error=%q", got)
	}
	if (&APIError{Kind: apiErrorKindAPI}).Is(errors.New("other")) {
		t.Fatal("APIError Is matched unrelated error")
	}
	if (*APIError)(nil).Is(ErrAuthentication) {
		t.Fatal("nil APIError Is returned true")
	}

	err := apiErrorFromStatus(http.StatusForbidden, "")
	if !errors.Is(err, ErrAuthentication) || err.Message != "HTTP 403" {
		t.Fatalf("forbidden error=%#v", err)
	}
	if !errors.Is(apiErrorFromStatus(http.StatusInternalServerError, "sandbox not found downstream"), ErrSandboxNotFound) {
		t.Fatal("sandbox not found message was not classified")
	}
	if errors.Is(apiErrorFromStatus(http.StatusInternalServerError, "unrelated not found"), ErrSandboxNotFound) {
		t.Fatal("unrelated not found message classified as sandbox not found")
	}

	if got := readErrorMessage(nil); got != "" {
		t.Fatalf("nil response message=%q", got)
	}
	if got := readErrorMessage(&http.Response{}); got != "" {
		t.Fatalf("nil body message=%q", got)
	}
	if got := readErrorMessage(&http.Response{Body: io.NopCloser(strings.NewReader("   "))}); got != "" {
		t.Fatalf("blank body message=%q", got)
	}
	if got := readErrorMessage(&http.Response{Body: io.NopCloser(strings.NewReader(`{"detail":"detail msg"}`))}); got != "detail msg" {
		t.Fatalf("detail message=%q", got)
	}
	if got := readErrorMessage(&http.Response{Body: io.NopCloser(strings.NewReader(`{"message":7}`))}); got != `{"message":7}` {
		t.Fatalf("numeric message body=%q", got)
	}
	if got := readErrorMessage(&http.Response{Body: errReaderCloser{}}); got != "" {
		t.Fatalf("read error message=%q", got)
	}
}

func TestParseLineMalformedTypedEventsAndTracebackEdges(t *testing.T) {
	execution := &Execution{}
	parseLine(execution, nil, RunCodeOptions{})
	parseLine(execution, []byte(`{"type":7}`), RunCodeOptions{})
	parseLine(execution, []byte(`{"type":"result","text":{}}`), RunCodeOptions{})
	parseLine(execution, []byte(`{"type":"stdout","text":{}}`), RunCodeOptions{})
	parseLine(execution, []byte(`{"type":"stderr","text":{}}`), RunCodeOptions{})
	parseLine(execution, []byte(`{"type":"error","traceback":{}}`), RunCodeOptions{})
	parseLine(execution, []byte(`{"type":"error","name":{}}`), RunCodeOptions{})
	parseLine(execution, []byte(`{"type":"number_of_executions","execution_count":"bad"}`), RunCodeOptions{})

	if len(execution.Results) != 0 || len(execution.Logs.Stdout) != 0 || len(execution.Logs.Stderr) != 0 || execution.ExecutionCount != nil {
		t.Fatalf("malformed events changed execution: %#v", execution)
	}
	if execution.Error == nil || len(execution.Error.Traceback) != 0 {
		t.Fatalf("malformed error event mismatch: %#v", execution.Error)
	}

	if got := parseTraceback(nil); got != nil {
		t.Fatalf("nil traceback=%#v", got)
	}
	if got := parseTraceback([]byte(`null`)); got != nil {
		t.Fatalf("null traceback=%#v", got)
	}
	if got := parseTraceback([]byte(`""`)); got != nil {
		t.Fatalf("empty string traceback=%#v", got)
	}
	if got := parseTraceback([]byte(`{}`)); got != nil {
		t.Fatalf("object traceback=%#v", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type closeCountingTransport struct {
	closes int
}

func (t *closeCountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

func (t *closeCountingTransport) CloseIdleConnections() {
	t.closes++
}

type errReaderCloser struct{}

func (errReaderCloser) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

func (errReaderCloser) Close() error {
	return nil
}

type cancelOnCloseBody struct {
	*strings.Reader
	cancel context.CancelFunc
}

func (b cancelOnCloseBody) Close() error {
	b.cancel()
	return nil
}

func TestReadConnectEnvelopeInvalidFlagsDiagnostic(t *testing.T) {
	// HTML responses have an invalid flags byte, regardless of prefix or encoding.
	for _, response := range []string{
		"<!DOCTYPE html>\n<html><body>Web UI</body></html>",
		"\xef\xbb\xbf<!DOCTYPE html>",
		"\n<html><body>Web UI</body></html>",
		"{\"error\":\"bad route\"}",
		"\x1f\x8b\x08\x00compressed response",
	} {
		_, _, err := readConnectEnvelope(strings.NewReader(response))
		if err == nil {
			t.Fatalf("readConnectEnvelope(%q) returned nil error", response[:min(12, len(response))])
		}
		if !strings.Contains(err.Error(), "not a Connect envelope") {
			t.Fatalf("err=%v, want invalid envelope diagnostic", err)
		}
		if !strings.Contains(err.Error(), "CUBE_PROXY_NODE_IP") {
			t.Fatalf("err=%v, want CUBE_PROXY_NODE_IP hint", err)
		}
	}

	// A valid compressed + end-stream flags combination is allowed.
	valid := []byte{connectCompressedFlag | connectEndStreamFlag, 0, 0, 0, 0}
	if _, _, err := readConnectEnvelope(bytes.NewReader(valid)); err != nil {
		t.Fatalf("readConnectEnvelope(valid flags)=%v, want nil", err)
	}

	// Oversized frames with valid flags retain the size diagnostic.
	oversized := make([]byte, 5)
	oversized[0] = connectEndStreamFlag
	oversized[1] = 0x05 // size > 64MiB
	_, _, err := readConnectEnvelope(bytes.NewReader(oversized))
	if err == nil {
		t.Fatal("readConnectEnvelope with oversized frame returned nil error")
	}
	if !strings.Contains(err.Error(), "Connect stream message too large") {
		t.Fatalf("err=%v, want Connect stream message too large", err)
	}
}

func TestValidateConnectResponse(t *testing.T) {
	if err := validateConnectResponse(nil); err == nil || err.Error() != "nil response" {
		t.Fatalf("validateConnectResponse(nil)=%v, want nil response", err)
	}
	if err := validateConnectResponse(&http.Response{Body: nil}); err == nil || err.Error() != "nil response" {
		t.Fatalf("validateConnectResponse(Body:nil)=%v, want nil response", err)
	}

	// Non-200 JSON
	resp404 := &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"message":"not found"}`)),
	}
	if err := validateConnectResponse(resp404); err == nil {
		t.Fatal("validateConnectResponse(404) returned nil error")
	}

	// Non-200 HTML error page (e.g. gateway 502/404)
	resp502HTML := &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader("<!DOCTYPE html><html><body>502 Bad Gateway</body></html>")),
	}
	err502 := validateConnectResponse(resp502HTML)
	if err502 == nil {
		t.Fatal("validateConnectResponse(502 HTML) returned nil error")
	}
	var apiErr *APIError
	if !errors.As(err502, &apiErr) {
		t.Fatalf("err502=%v, want *APIError", err502)
	}
	if apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("StatusCode=%d, want 502", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Message, "502 Bad Gateway") || !strings.Contains(apiErr.Message, "CUBE_PROXY_NODE_IP") {
		t.Fatalf("message=%q, want original error and CUBE_PROXY_NODE_IP hint", apiErr.Message)
	}

	// 401 HTML preserves ErrAuthentication
	resp401HTML := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       io.NopCloser(strings.NewReader("<html><body>login</body></html>")),
	}
	err401 := validateConnectResponse(resp401HTML)
	if !errors.Is(err401, ErrAuthentication) {
		t.Fatalf("validateConnectResponse(401 HTML)=%v, want ErrAuthentication", err401)
	}

	// 404 HTML clears ErrSandboxNotFound
	resp404HTML := &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       io.NopCloser(strings.NewReader("<html><body>404 not found</body></html>")),
	}
	err404HTML := validateConnectResponse(resp404HTML)
	if errors.Is(err404HTML, ErrSandboxNotFound) {
		t.Fatalf("validateConnectResponse(404 HTML)=%v, should not be ErrSandboxNotFound", err404HTML)
	}

	// 404 text/plain (the default response from net/http.NotFound) also clears it.
	resp404Text := &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader("404 page not found\n")),
	}
	err404Text := validateConnectResponse(resp404Text)
	if errors.Is(err404Text, ErrSandboxNotFound) || !strings.Contains(err404Text.Error(), "CUBE_PROXY_NODE_IP") {
		t.Fatalf("validateConnectResponse(404 text/plain)=%v, should clear classification and include routing hint", err404Text)
	}
	if !strings.Contains(err404Text.Error(), "404 page not found") {
		t.Fatalf("validateConnectResponse(404 text/plain)=%v, should preserve original error", err404Text)
	}

	// Large HTML error page (>200 chars) is truncated before appending the hint
	longHTML := "<!DOCTYPE html><html><body>" + strings.Repeat("A", 300) + "</body></html>"
	respLongHTML := &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader(longHTML)),
	}
	errLongHTML := validateConnectResponse(respLongHTML)
	if errLongHTML == nil {
		t.Fatal("validateConnectResponse(long HTML) returned nil error")
	}
	var apiErrLong *APIError
	if !errors.As(errLongHTML, &apiErrLong) {
		t.Fatalf("errLongHTML=%v, want *APIError", errLongHTML)
	}
	if !strings.Contains(apiErrLong.Message, "...; response may be an HTML/text page") {
		t.Fatalf("expected truncated message with '...', got: %q", apiErrLong.Message)
	}
	rawPrefix := strings.Split(apiErrLong.Message, "...; response may be")[0]
	if len(rawPrefix) != 200 {
		t.Fatalf("expected truncated prefix length 200, got %d (%q)", len(rawPrefix), rawPrefix)
	}

	for _, tc := range []struct {
		body   string
		target error
	}{
		{`{"error":"template not found"}`, ErrTemplateNotFound},
		{`{"error":"volume not found: vol-1"}`, ErrVolumeNotFound},
	} {
		resp := &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(tc.body)),
		}
		if err := validateConnectResponse(resp); !errors.Is(err, tc.target) {
			t.Errorf("validateConnectResponse(404 %q)=%v, want %v", tc.body, err, tc.target)
		} else if strings.Contains(err.Error(), "CUBE_PROXY_NODE_IP") {
			t.Errorf("validateConnectResponse(404 %q)=%v, should not suggest proxy routing", tc.body, err)
		}
	}

	// 404 HTML pages that happen to mention "template" or "volume" are still routing pages
	resp404HTMLTemplate := &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       io.NopCloser(strings.NewReader("<html><title>Template not found</title></html>")),
	}
	err404HTMLTemplate := validateConnectResponse(resp404HTMLTemplate)
	if errors.Is(err404HTMLTemplate, ErrTemplateNotFound) || !strings.Contains(err404HTMLTemplate.Error(), "CUBE_PROXY_NODE_IP") {
		t.Fatalf("validateConnectResponse(404 HTML template)=%v, want cleared classification and proxy hint", err404HTMLTemplate)
	}

	// 404 text pages mentioning template are also routing pages
	resp404TextTemplate := &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": []string{"text/plain"}},
		Body:       io.NopCloser(strings.NewReader("template not found")),
	}
	err404TextTemplate := validateConnectResponse(resp404TextTemplate)
	if errors.Is(err404TextTemplate, ErrTemplateNotFound) || !strings.Contains(err404TextTemplate.Error(), "CUBE_PROXY_NODE_IP") {
		t.Fatalf("validateConnectResponse(404 text template)=%v, want cleared classification and proxy hint", err404TextTemplate)
	}

	resp302 := &http.Response{
		StatusCode: http.StatusFound,
		Header:     http.Header{"Location": []string{"https://portal.example/login"}},
		Body:       io.NopCloser(strings.NewReader("")),
	}
	if err := validateConnectResponse(resp302); err == nil || !strings.Contains(err.Error(), "https://portal.example/login") {
		t.Fatalf("validateConnectResponse(302)=%v, want redirect location", err)
	}

	resp500Text := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader("boom\n")),
	}
	if err := validateConnectResponse(resp500Text); err == nil || err.Error() != "boom (HTTP 500)" {
		t.Fatalf("validateConnectResponse(500 text/plain)=%v, want original server error", err)
	}

	resp404JSON := &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"message":"sandbox not found"}`)),
	}
	if err := validateConnectResponse(resp404JSON); !errors.Is(err, ErrSandboxNotFound) {
		t.Fatalf("validateConnectResponse(404 JSON)=%v, want ErrSandboxNotFound", err)
	}

	// 200 with text/html returns *APIError
	respHTML := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader("<!DOCTYPE html><html></html>")),
	}
	errHTML := validateConnectResponse(respHTML)
	if errHTML == nil || !strings.Contains(errHTML.Error(), "received HTML") {
		t.Fatalf("validateConnectResponse(text/html)=%v, want HTML diagnostic", errHTML)
	}
	var apiErr200 *APIError
	if !errors.As(errHTML, &apiErr200) || apiErr200.StatusCode != 0 {
		t.Fatalf("validateConnectResponse(text/html) should return *APIError without an HTTP error status, got: %v", errHTML)
	}

	// 200 with text/plain falls through to envelope reader (tolerant of proxies rewriting Content-Type)
	respPlain := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/plain"}},
		Body:       io.NopCloser(strings.NewReader("error page")),
	}
	if err := validateConnectResponse(respPlain); err != nil {
		t.Fatalf("validateConnectResponse(text/plain)=%v, want nil", err)
	}

	// 200 with valid Connect content-type
	respConnect := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/connect+json"}},
		Body:       io.NopCloser(strings.NewReader("")),
	}
	if err := validateConnectResponse(respConnect); err != nil {
		t.Fatalf("validateConnectResponse(application/connect+json)=%v, want nil", err)
	}

	// 200 with mixed-case Connect content-type and parameters
	respMixedCaseConnect := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"Application/Connect+JSON; charset=utf-8"}},
		Body:       io.NopCloser(strings.NewReader("")),
	}
	if err := validateConnectResponse(respMixedCaseConnect); err != nil {
		t.Fatalf("validateConnectResponse(Application/Connect+JSON)=%v, want nil", err)
	}

	// 200 with omitted Content-Type falls through (tolerant path)
	respOmittedCT := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
	}
	if err := validateConnectResponse(respOmittedCT); err != nil {
		t.Fatalf("validateConnectResponse(omitted Content-Type)=%v, want nil", err)
	}

	// 200 with application/json falls through to envelope reader (tolerant path)
	respJSON := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader("")),
	}
	if err := validateConnectResponse(respJSON); err != nil {
		t.Fatalf("validateConnectResponse(application/json)=%v, want nil", err)
	}
}

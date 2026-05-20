// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
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
	if err := (&Sandbox{}).Resume(ctx, 0); err == nil {
		t.Fatal("Resume without client returned nil error")
	}
	if err := (&Sandbox{}).Kill(ctx); err == nil {
		t.Fatal("Kill without client returned nil error")
	}
	if _, err := (&Sandbox{}).RunCode(ctx, "1", RunCodeOptions{}); err == nil {
		t.Fatal("RunCode without client returned nil error")
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
	if err := sb.Resume(context.Background(), 0); err != nil {
		t.Fatalf("Resume returned error: %v", err)
	}
	if !strings.Contains(gotBody, `"timeout":123`) {
		t.Fatalf("resume body=%s", gotBody)
	}

	sb.client.config.APIURL = "http://%"
	if err := sb.Resume(context.Background(), time.Second); err == nil {
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

func TestCloseBranchesAndAccessors(t *testing.T) {
	if err := (&Sandbox{}).Close(); err != nil {
		t.Fatalf("Close without client returned error: %v", err)
	}
	sb := &Sandbox{client: NewClient(Config{})}
	if err := sb.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
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

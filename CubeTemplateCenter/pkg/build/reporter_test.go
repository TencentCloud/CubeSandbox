// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package build

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The reporter is TC's only channel back to CubeMaster. Losing a terminal
// report leaves the job stuck in RUNNING until the reconciler times it out
// (hours), while retrying something CubeMaster has already rejected just burns
// the build goroutine. The retry classification is therefore the contract, and
// these tests pin it.

// newTestReporter points a reporter at srv and shrinks the backoff so the retry
// budget can be exercised in milliseconds instead of ~40s.
func newTestReporter(t *testing.T, srv *httptest.Server) *Reporter {
	t.Helper()

	origBase, origMax := reportBaseBackoff, reportMaxBackoff
	reportBaseBackoff = time.Millisecond
	reportMaxBackoff = 2 * time.Millisecond
	t.Cleanup(func() {
		reportBaseBackoff, reportMaxBackoff = origBase, origMax
	})

	return &Reporter{
		masterURL:  strings.TrimRight(srv.URL, "/"),
		httpClient: srv.Client(),
	}
}

func TestReportSucceedsFirstTry(t *testing.T) {
	var calls int32
	var gotPath string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newTestReporter(t, srv)
	err := r.Report(context.Background(), "job-abc", map[string]any{
		"status":   "RUNNING",
		"progress": 42,
	})
	if err != nil {
		t.Fatalf("Report() returned %v, want nil", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("server saw %d call(s), want exactly 1", got)
	}
	// The job id must land in the path: CubeMaster routes on it, and a
	// misplaced id would be reported as a 404 the reporter would not retry.
	if want := "/internal/template/jobs/job-abc/status"; gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
	if gotBody["status"] != "RUNNING" {
		t.Fatalf("body did not carry the status: %+v", gotBody)
	}
}

func TestReportRetriesTransientFailuresThenSucceeds(t *testing.T) {
	// The point of retrying: a CubeMaster rolling restart must not lose a
	// report.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newTestReporter(t, srv)
	if err := r.Report(context.Background(), "job-abc", map[string]any{"status": "RUNNING"}); err != nil {
		t.Fatalf("Report() returned %v, want nil after the server recovered", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("server saw %d call(s), want 3 (two failures then a success)", got)
	}
}

func TestReportRetryClassification(t *testing.T) {
	// Retrying a 4xx cannot help: it means the payload or the route is wrong,
	// which is a bug to surface immediately rather than hammer.
	tests := []struct {
		name       string
		status     int
		fields     map[string]any
		wantCalls  int32
		wantErrHas string
	}{
		{
			name: "400-is-permanent", status: http.StatusBadRequest,
			fields: map[string]any{"status": "RUNNING"}, wantCalls: 1,
			wantErrHas: "master returned 400",
		},
		{
			name: "404-is-permanent", status: http.StatusNotFound,
			fields: map[string]any{"status": "RUNNING"}, wantCalls: 1,
			wantErrHas: "master returned 404",
		},
		{
			name: "409-is-permanent", status: http.StatusConflict,
			fields: map[string]any{"status": "RUNNING"}, wantCalls: 1,
			wantErrHas: "master returned 409",
		},
		{
			// 429 is the one 4xx worth retrying: it is explicitly "try later".
			name: "429-is-retryable", status: http.StatusTooManyRequests,
			fields: map[string]any{"status": "RUNNING"}, wantCalls: reportMaxAttempts,
			wantErrHas: "gave up",
		},
		{
			name: "500-is-retryable", status: http.StatusInternalServerError,
			fields: map[string]any{"status": "RUNNING"}, wantCalls: reportMaxAttempts,
			wantErrHas: "gave up",
		},
		{
			name: "503-is-retryable", status: http.StatusServiceUnavailable,
			fields: map[string]any{"status": "RUNNING"}, wantCalls: reportMaxAttempts,
			wantErrHas: "gave up",
		},
		{
			// A terminal report is worth a much larger budget, because losing it
			// strands the job in RUNNING.
			name: "terminal-report-retries-harder", status: http.StatusServiceUnavailable,
			fields: map[string]any{"status": "BUILT"}, wantCalls: terminalReportMaxAttempts,
			wantErrHas: "gave up",
		},
		{
			name: "terminal-failed-retries-harder", status: http.StatusServiceUnavailable,
			fields: map[string]any{"status": "FAILED"}, wantCalls: terminalReportMaxAttempts,
			wantErrHas: "gave up",
		},
		{
			// A permanent error must stop immediately even for a terminal
			// report: the larger budget is about transient faults only.
			name: "terminal-report-still-stops-on-4xx", status: http.StatusBadRequest,
			fields: map[string]any{"status": "BUILT"}, wantCalls: 1,
			wantErrHas: "master returned 400",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			r := newTestReporter(t, srv)
			err := r.Report(context.Background(), "job-abc", tc.fields)
			if err == nil {
				t.Fatalf("Report() returned nil, want an error for status %d", tc.status)
			}
			if !strings.Contains(err.Error(), tc.wantErrHas) {
				t.Fatalf("Report() error = %q, want it to contain %q", err, tc.wantErrHas)
			}
			if got := atomic.LoadInt32(&calls); got != tc.wantCalls {
				t.Fatalf("server saw %d call(s), want %d", got, tc.wantCalls)
			}
		})
	}
}

func TestReportPropagatesMasterErrorBody(t *testing.T) {
	// The body is the only explanation TC can log for a rejected report, so it
	// must survive into the error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("unknown column artifact_size"))
	}))
	defer srv.Close()

	r := newTestReporter(t, srv)
	err := r.Report(context.Background(), "job-abc", map[string]any{"status": "RUNNING"})
	if err == nil || !strings.Contains(err.Error(), "unknown column artifact_size") {
		t.Fatalf("Report() error = %v, want it to carry the master's response body", err)
	}
}

func TestReportStopsOnCanceledContext(t *testing.T) {
	// The build context is canceled when TC shuts down. Continuing to retry
	// would keep the process alive past its deadline.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	r := newTestReporter(t, srv)

	// Cancel while the reporter is between attempts.
	go func() {
		time.Sleep(2 * time.Millisecond)
		cancel()
	}()

	err := r.Report(ctx, "job-abc", map[string]any{"status": "BUILT"})
	if err == nil {
		t.Fatalf("Report() returned nil, want an error once the context was canceled")
	}
	if got := atomic.LoadInt32(&calls); got >= terminalReportMaxAttempts {
		t.Fatalf("server saw %d call(s); cancellation must cut the retry budget short", got)
	}
}

func TestReportWithAlreadyCanceledContextDoesNotRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := newTestReporter(t, srv)
	if err := r.Report(ctx, "job-abc", map[string]any{"status": "BUILT"}); err == nil {
		t.Fatalf("Report() returned nil, want an error for an already-canceled context")
	}
	// A dead context is a permanent condition, not a transient one.
	if got := atomic.LoadInt32(&calls); got > 1 {
		t.Fatalf("server saw %d call(s), want at most 1 for a dead context", got)
	}
}

func TestReportRejectsUnmarshalableFields(t *testing.T) {
	// A payload that cannot be encoded must fail before any request is made,
	// rather than being retried 12 times.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newTestReporter(t, srv)
	err := r.Report(context.Background(), "job-abc", map[string]any{
		"status": "BUILT",
		"bad":    math.Inf(1), // JSON has no infinity
	})
	if err == nil || !strings.Contains(err.Error(), "marshal status") {
		t.Fatalf("Report() error = %v, want a marshal failure", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("server saw %d call(s), want 0 — the payload never became valid", got)
	}
}

func TestReportTransportErrorIsRetried(t *testing.T) {
	// A closed server stands in for CubeMaster being down: the connection is
	// refused, which is exactly the transient case retries exist for.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	client := srv.Client()
	url := srv.URL
	srv.Close()

	origBase, origMax := reportBaseBackoff, reportMaxBackoff
	reportBaseBackoff, reportMaxBackoff = time.Millisecond, 2*time.Millisecond
	defer func() { reportBaseBackoff, reportMaxBackoff = origBase, origMax }()

	r := &Reporter{masterURL: url, httpClient: client}
	err := r.Report(context.Background(), "job-abc", map[string]any{"status": "RUNNING"})
	if err == nil {
		t.Fatalf("Report() returned nil, want an error when master is unreachable")
	}
	// "gave up after N attempts" proves the transport error was classified as
	// retryable rather than permanent.
	if !strings.Contains(err.Error(), "gave up") {
		t.Fatalf("Report() error = %q, want it to report exhausted retries", err)
	}
}

func TestIsTerminalReport(t *testing.T) {
	// This selects the retry budget, so the parsing must tolerate whatever
	// casing the build pipeline happens to use.
	tests := []struct {
		name   string
		fields map[string]any
		want   bool
	}{
		{name: "built", fields: map[string]any{"status": "BUILT"}, want: true},
		{name: "failed", fields: map[string]any{"status": "FAILED"}, want: true},
		{name: "ready", fields: map[string]any{"status": "READY"}, want: true},
		{name: "lowercase-built", fields: map[string]any{"status": "built"}, want: true},
		{name: "mixed-case-failed", fields: map[string]any{"status": "Failed"}, want: true},
		{name: "padded-built", fields: map[string]any{"status": "  BUILT  "}, want: true},

		{name: "running", fields: map[string]any{"status": "RUNNING"}},
		{name: "pending", fields: map[string]any{"status": "PENDING"}},
		{name: "empty-status", fields: map[string]any{"status": ""}},
		{name: "missing-status", fields: map[string]any{"progress": 50}},
		{name: "nil-fields", fields: nil},
		// A non-string status is a caller bug; treating it as terminal would
		// silently triple the retry budget.
		{name: "non-string-status", fields: map[string]any{"status": 200}},
		{name: "unknown-status", fields: map[string]any{"status": "BUILDING_EXT4"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTerminalReport(tc.fields); got != tc.want {
				t.Fatalf("isTerminalReport(%+v) = %v, want %v", tc.fields, got, tc.want)
			}
		})
	}
}

func TestNewReporterEndpointResolution(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want string
	}{
		{name: "default", env: "", want: "http://localhost:8089"},
		{name: "explicit", env: "http://cubemaster:8089", want: "http://cubemaster:8089"},
		// A trailing slash would produce a double slash in the callback path.
		{name: "trailing-slash-trimmed", env: "http://cubemaster:8089/", want: "http://cubemaster:8089"},
		{name: "multiple-trailing-slashes", env: "http://cubemaster:8089///", want: "http://cubemaster:8089"},
		{name: "whitespace-padded", env: "  http://cubemaster:8089  ", want: "http://cubemaster:8089"},
		{name: "whitespace-only-falls-back", env: "   ", want: "http://localhost:8089"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CUBE_MASTER_ENDPOINT", tc.env)
			r := NewReporter()
			defer r.Close()
			if r.masterURL != tc.want {
				t.Fatalf("NewReporter().masterURL = %q, want %q", r.masterURL, tc.want)
			}
		})
	}
}

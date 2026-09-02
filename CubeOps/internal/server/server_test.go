// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	cubelog "github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
)

// uuidV4Pattern matches the canonical UUID layout from uuid.NewString().
// Shape-only: sufficient to tell "fresh UUID" from "the rejected input".
var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// newTestEngine builds a minimal engine running only requestLogger, enough
// to exercise request-id / caller / probe-skip-stat logic without a full
// server. The handler records the middleware-derived caller into *seenCaller.
func newTestEngine(t *testing.T) (*gin.Engine, *string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(requestLogger())

	seenCaller := ""
	r.GET("/*any", func(c *gin.Context) {
		if rt := cubelog.GetTraceInfo(c.Request.Context()); rt != nil {
			seenCaller = rt.Caller
		}
	})
	return r, &seenCaller
}

// captureTrace redirects cubelog's stat writer to a buffer for the test.
// Cleanup restores nil (the pre-test default) and disables LogMetric so
// later tests do not leak trace lines to stderr.
type traceCapture struct {
	buf *bytes.Buffer
}

func captureTrace(t *testing.T) *traceCapture {
	t.Helper()
	var buf bytes.Buffer
	cubelog.SetTraceOutput(&buf)
	cubelog.EnableLogMetric()
	tc := &traceCapture{buf: &buf}
	t.Cleanup(func() {
		cubelog.SetTraceOutput(io.Discard)
		cubelog.DisableLogMetric()
		cubelog.SetTraceOutput(nil)
	})
	return tc
}

// captureOutput redirects the business log writer to a buffer for the test.
type outputCapture struct {
	buf *bytes.Buffer
}

func captureOutput(t *testing.T) *outputCapture {
	t.Helper()
	var buf bytes.Buffer
	cubelog.SetOutput(&buf)
	oc := &outputCapture{buf: &buf}
	t.Cleanup(func() { cubelog.SetOutput(nil) })
	return oc
}

// TestRequestLogger_RejectsInvalidXRequestID: a bad X-RequestID falls back
// to a fresh UUID and is echoed in the response header.
func TestRequestLogger_RejectsInvalidXRequestID(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{"path traversal", "../../../etc/passwd"},
		{"contains space", "rid with space"},
		{"contains slash", "rid/abc"},
		{"too long", strings.Repeat("a", 129)},
		{"empty after trim", "   "},
		{"non-ascii", "rid-中文"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestEngine(t)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/anything", nil)
			req.Header.Set("X-RequestID", tc.header)

			r.ServeHTTP(w, req)

			got := w.Header().Get("X-RequestID")
			if !uuidV4Pattern.MatchString(got) {
				t.Errorf("X-RequestID = %q, want a fresh UUID (header was %q)", got, tc.header)
			}
			if tc.header != "" && strings.Contains(got, tc.header) {
				t.Errorf("X-RequestID %q leaks the rejected input %q", got, tc.header)
			}
		})
	}
}

// TestRequestLogger_EchoesValidXRequestID: a valid X-RequestID is preserved
// verbatim and echoed in the response header.
func TestRequestLogger_EchoesValidXRequestID(t *testing.T) {
	const rid = "rid-abc.123_DEF"
	r, _ := newTestEngine(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("X-RequestID", rid)

	r.ServeHTTP(w, req)

	if got := w.Header().Get("X-RequestID"); got != rid {
		t.Errorf("X-RequestID = %q, want %q", got, rid)
	}
}

// TestRequestLogger_AcceptsAlternateHeaderName: X-Request-ID (hyphenated)
// is also honoured.
func TestRequestLogger_AcceptsAlternateHeaderName(t *testing.T) {
	const rid = "rid-from-alt-header"
	r, _ := newTestEngine(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("X-Request-ID", rid)

	r.ServeHTTP(w, req)

	if got := w.Header().Get("X-RequestID"); got != rid {
		t.Errorf("X-RequestID = %q, want %q", got, rid)
	}
}

// TestRequestLogger_HealthProbeSkipsStat: /health must not emit a Trace()
// line, otherwise liveness probes flood the stat log.
func TestRequestLogger_HealthProbeSkipsStat(t *testing.T) {
	tc := captureTrace(t)

	r, seenCaller := newTestEngine(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)

	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if *seenCaller != "probe" {
		t.Errorf("caller = %q, want \"probe\"", *seenCaller)
	}
	if tc.buf.Len() != 0 {
		t.Errorf("stat writer captured %d bytes for /health; probe must skip Trace():\n%s",
			tc.buf.Len(), tc.buf.String())
	}
}

// TestRequestLogger_NonProbeEmitsStat: non-probe requests must emit
// Trace(), otherwise the probe-skip branch could silently widen.
func TestRequestLogger_NonProbeEmitsStat(t *testing.T) {
	tc := captureTrace(t)

	r, seenCaller := newTestEngine(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/anything", nil)
	req.Header.Set("X-Caller", "webui")

	r.ServeHTTP(w, req)

	if *seenCaller != "webui" {
		t.Errorf("caller = %q, want \"webui\"", *seenCaller)
	}
	if tc.buf.Len() == 0 {
		t.Errorf("stat writer captured 0 bytes; non-probe must emit Trace()")
	}
}

// TestRequestLogger_RejectsInvalidCaller: a bad X-Caller falls back to the
// "cubeops-client" sentinel.
func TestRequestLogger_RejectsInvalidCaller(t *testing.T) {
	cases := []string{
		"caller with space",
		"caller/with/slash",
		strings.Repeat("c", 129),
		"caller-中文",
	}
	for _, bad := range cases {
		t.Run(bad, func(t *testing.T) {
			r, seenCaller := newTestEngine(t)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/anything", nil)
			req.Header.Set("X-Caller", bad)

			r.ServeHTTP(w, req)

			if *seenCaller != "cubeops-client" {
				t.Errorf("caller = %q, want \"cubeops-client\" (input was %q)", *seenCaller, bad)
			}
		})
	}
}

// TestRequestLogger_MissingCallerFallsBackToDefault: absent X-Caller uses
// the sentinel, not the empty string.
func TestRequestLogger_MissingCallerFallsBackToDefault(t *testing.T) {
	r, seenCaller := newTestEngine(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/anything", nil)

	r.ServeHTTP(w, req)

	if *seenCaller != "cubeops-client" {
		t.Errorf("caller = %q, want \"cubeops-client\"", *seenCaller)
	}
}

// TestCubeopsRecovery_PanicBecomes500: a handler panic is caught, returns
// 500, and the recovery log line lands in cubelog with the inbound
// RequestID attached.
func TestCubeopsRecovery_PanicBecomes500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(requestLogger())
	r.Use(cubeopsRecovery())
	r.GET("/boom", func(c *gin.Context) { panic("simulated handler panic") })

	oc := captureOutput(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	req.Header.Set("X-RequestID", "rid-recovery-1")

	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(oc.buf.String(), "panic recovered") {
		t.Errorf("expected cubelog to record \"panic recovered\"; got:\n%s", oc.buf.String())
	}
	if !strings.Contains(oc.buf.String(), "rid-recovery-1") {
		t.Errorf("panic log missing the inbound RequestID; got:\n%s", oc.buf.String())
	}
}

// TestCubeopsRecovery_NoDoubleWriteWhenHandlerAlreadyWrote: if the handler
// already wrote part of the body, recovery must not overwrite the status.
func TestCubeopsRecovery_NoDoubleWriteWhenHandlerAlreadyWrote(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(requestLogger())
	r.Use(cubeopsRecovery())
	r.GET("/partial", func(c *gin.Context) {
		c.Status(http.StatusOK)
		_, _ = c.Writer.Write([]byte("partial body"))
		panic("panic after partial write")
	})

	oc := captureOutput(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/partial", nil)

	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (handler already wrote)", w.Code)
	}
	if !strings.Contains(oc.buf.String(), "panic recovered") {
		t.Errorf("expected cubelog to record \"panic recovered\"; got:\n%s", oc.buf.String())
	}
}

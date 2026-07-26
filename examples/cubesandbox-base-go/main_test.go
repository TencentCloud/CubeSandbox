package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleRootReturnsOKWithExpectedContent(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	handleRootForPort(rr, req, "8080")

	if rr.Code != http.StatusOK {
		t.Fatalf("handleRoot status = %d, want %d", rr.Code, http.StatusOK)
	}

	ct := rr.Header().Get("Content-Type")
	if ct != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want %q", ct, "text/html; charset=utf-8")
	}

	body := rr.Body.String()
	for _, want := range []string{
		"cubesandbox-base-go",
		"Hello from Go inside a CubeSandbox MicroVM",
		"Go runtime:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("response body missing %q\nbody:\n%s", want, body)
		}
	}
}

func TestHandleRootEchoesAppPortFromEnv(t *testing.T) {
	t.Setenv("APP_PORT", "9999")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	handleRootForPort(rr, req, configuredPort())

	if !strings.Contains(rr.Body.String(), ":9999") {
		t.Errorf("body should contain the APP_PORT value 9999\nbody:\n%s", rr.Body.String())
	}
}

func TestHandleRootDefaultsToPort8080(t *testing.T) {
	t.Setenv("APP_PORT", "")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	handleRootForPort(rr, req, configuredPort())

	if !strings.Contains(rr.Body.String(), ":8080") {
		t.Errorf("body should contain default port 8080\nbody:\n%s", rr.Body.String())
	}
}

func TestConfiguredPortFallsBackOnInvalidValue(t *testing.T) {
	for _, value := range []string{"abc", "0", "65536", "-1"} {
		t.Setenv("APP_PORT", value)

		if port := configuredPort(); port != "8080" {
			t.Errorf("configuredPort() with APP_PORT=%q = %q, want %q", value, port, "8080")
		}
	}
}

func TestHandleHealthReturnsOKWithEmptyBody(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)

	handleHealth(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("handleHealth status = %d, want %d", rr.Code, http.StatusOK)
	}

	body, err := io.ReadAll(rr.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("handleHealth body = %q, want empty", body)
	}
}

func TestHandleHealthIgnoresMethod(t *testing.T) {
	for _, method := range []string{
		http.MethodGet,
		http.MethodPost,
		http.MethodHead,
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/health", nil)

		handleHealth(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("method %s: status = %d, want %d", method, rr.Code, http.StatusOK)
		}
	}
}

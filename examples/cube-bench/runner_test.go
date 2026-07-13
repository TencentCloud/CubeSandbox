package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBenchOneSendsHostMountMetadata(t *testing.T) {
	rawHostMount := `[
		{"hostPath":"/tmp/data","mountPath":"/mnt/data","readOnly":false}
	]`
	hostMountValue, err := prepareHostMount(rawHostMount)
	if err != nil {
		t.Fatalf("prepareHostMount returned error: %v", err)
	}
	requestBody, err := buildCreateRequestBody("tpl-test", hostMountValue)
	if err != nil {
		t.Fatalf("buildCreateRequestBody returned error: %v", err)
	}

	var got map[string]any
	handlerErrCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sandboxes" {
			select {
			case handlerErrCh <- fmt.Errorf("request = %s %s", r.Method, r.URL.Path):
			default:
			}
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			select {
			case handlerErrCh <- fmt.Errorf("Authorization=%q, want Bearer test-key", auth):
			default:
			}
			http.Error(w, "unexpected auth", http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			select {
			case handlerErrCh <- fmt.Errorf("decode body: %v", err):
			default:
			}
			http.Error(w, "decode body failed", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"sandboxID":"sb-test-001"}`)
	}))
	defer server.Close()

	cfg := &Config{
		Template:       "tpl-test",
		Mode:           "create-only",
		APIURL:         server.URL,
		APIKey:         "test-key",
		HostMount:      rawHostMount,
		hostMountValue: hostMountValue,
		requestBody:    requestBody,
		requestHeaders: map[string]string{"Authorization": "Bearer test-key"},
	}

	result := benchOne(server.Client(), cfg, 1)
	if result.Err != "" {
		t.Fatalf("benchOne returned error: %s", result.Err)
	}
	select {
	case err := <-handlerErrCh:
		t.Fatal(err)
	default:
	}

	metadata, ok := got["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata=%#v, want map[string]any", got["metadata"])
	}
	wantHostMount := `[{"hostPath":"/tmp/data","mountPath":"/mnt/data","readOnly":false}]`
	if got := metadata["host-mount"]; got != wantHostMount {
		t.Fatalf("metadata.host-mount=%#v, want %q", got, wantHostMount)
	}
}

func TestBenchOneDeletePath(t *testing.T) {
	requestBody, err := buildCreateRequestBody("tpl-delete", "")
	if err != nil {
		t.Fatalf("buildCreateRequestBody returned error: %v", err)
	}

	handlerErrCh := make(chan error, 1)
	deleteCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"sandboxID":"sb-delete-001"}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/sandboxes/sb-delete-001":
			deleteCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			select {
			case handlerErrCh <- fmt.Errorf("unexpected request = %s %s", r.Method, r.URL.Path):
			default:
			}
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	cfg := &Config{
		Template:       "tpl-delete",
		Mode:           "create-delete",
		APIURL:         server.URL,
		APIKey:         "test-key",
		requestBody:    requestBody,
		requestHeaders: map[string]string{"Authorization": "Bearer test-key"},
	}

	result := benchOne(server.Client(), cfg, 1)
	if result.Err != "" {
		t.Fatalf("benchOne returned error: %s", result.Err)
	}
	select {
	case err := <-handlerErrCh:
		t.Fatal(err)
	default:
	}
	if !deleteCalled {
		t.Fatal("benchOne did not issue the delete request")
	}
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/admission"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/podinformer"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/reporter"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/internal/store"
	"github.com/tencentcloud/CubeSandbox/eviction-webhook/pkg/types"
)

// TestE2EFullFlow verifies the end-to-end flow:
//
//	HTTPS AdmissionReview request → Handler parses → Stores locally → Reports to CubeMaster
//
// This test starts a real TLS webhook server and a fake CubeMaster HTTP server,
// sends a real AdmissionReview body, and asserts every step.
func TestE2EFullFlow(t *testing.T) {
	// ── 1. Fake CubeMaster for receiving reports ──
	var cubeMasterReceived atomic.Int32
	cubeMasterCapture := &requestCapture{}
	cubeSrv := startFakeCubeMaster(t, &cubeMasterReceived, cubeMasterCapture)
	defer cubeSrv.Close()

	// ── 2. Local audit store ──
	auditPath := filepath.Join(t.TempDir(), "e2e-audit.ndjson")
	auditStore, err := store.New(auditPath)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer auditStore.Close()

	// ── 3. Reporter pointing at fake CubeMaster (auth disabled for E2E) ──
	rep := reporter.New(cubeSrv.URL, "e2e-user", "e2e-secret", false, reporter.WithRetry(1, 10*time.Millisecond))

	// ── 4. Pod getter with predefined test data ──
	podGetter := &podinformer.Fake{Pods: map[string]*corev1.Pod{
		"cube-system/sandbox-e2e-001": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sandbox-e2e-001",
				Namespace: "cube-system",
				Labels: map[string]string{
					"cube.master.instance.type": "cubebox",
				},
			},
		},
	}}

	// ── 5. Admission handler ──
	handler := admission.New(podGetter, auditStore, rep)

	// ── 6. TLS webhook server ──
	tlsSrv := startTLSWebhookServer(t, handler)
	defer tlsSrv.Close()

	// ── 7. Send AdmissionReview via HTTPS ──
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID:       k8stypes.UID("e2e-uid-full-flow"),
			Name:      "sandbox-e2e-001",
			Namespace: "cube-system",
			UserInfo: authenticationv1.UserInfo{
				Username: "system:node:e2e-worker",
			},
		},
	}

	body, _ := json.Marshal(review)
	resp, err := httpsPost(tlsSrv.URL+"/webhook/eviction", body)
	if err != nil {
		t.Fatalf("POST /webhook/eviction: %v", err)
	}
	defer resp.Body.Close()

	// ── 8. Assert admission response ──
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	respBody, _ := io.ReadAll(resp.Body)
	var result admissionv1.AdmissionReview
	if err := json.Unmarshal(respBody, &result); err != nil {
		t.Fatalf("unmarshal response: %v\nbody=%s", err, string(respBody))
	}
	if result.Response == nil {
		t.Fatal("expected Response in AdmissionReview")
	}
	if result.Response.Allowed {
		t.Error("webhook must deny eviction (Allowed=false)")
	}
	if result.Response.UID != "e2e-uid-full-flow" {
		t.Errorf("UID: want e2e-uid-full-flow, got %q", result.Response.UID)
	}

	// ── 9. Wait for async report to complete ──
	deadline := time.After(2 * time.Second)
	for cubeMasterReceived.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for CubeMaster report")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	if cubeMasterReceived.Load() != 1 {
		t.Errorf("CubeMaster must receive exactly 1 report, got %d", cubeMasterReceived.Load())
	}

	// ── 10. Verify report payload ──
	var reported struct {
		EventID      string `json:"requestID"`
		PodName      string `json:"podName"`
		Namespace    string `json:"namespace"`
		NodeName     string `json:"nodeName"`
		InstanceType string `json:"instanceType"`
	}
	cubeMasterLastBody := cubeMasterCapture.lastBody()
	if err := json.Unmarshal(cubeMasterLastBody, &reported); err != nil {
		t.Fatalf("unmarshal CubeMaster body: %v\nbody=%s", err, string(cubeMasterLastBody))
	}
	if reported.EventID != "e2e-uid-full-flow" {
		t.Errorf("reported EventID: want e2e-uid-full-flow, got %s", reported.EventID)
	}
	if reported.PodName != "sandbox-e2e-001" {
		t.Errorf("reported PodName: want sandbox-e2e-001, got %s", reported.PodName)
	}
	if reported.NodeName != "e2e-worker" {
		t.Errorf("reported NodeName: want e2e-worker, got %s", reported.NodeName)
	}
	if reported.InstanceType != "cubebox" {
		t.Errorf("reported InstanceType: want cubebox, got %s", reported.InstanceType)
	}

	// ── 11. Verify local audit log ──
	auditData, _ := os.ReadFile(auditPath)
	if len(auditData) == 0 {
		t.Fatal("audit log is empty")
	}
}

// TestE2EEvictionAlwaysDenied verifies that the webhook returns Allowed=false
// for every eviction attempt regardless of Pod cache state.
func TestE2EEvictionAlwaysDenied(t *testing.T) {
	tests := []struct {
		name       string
		podInCache bool
	}{
		{"pod_in_cache", true},
		{"pod_not_in_cache", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var cubeMasterReceived atomic.Int32
			cubeSrv := startFakeCubeMaster(t, &cubeMasterReceived, &requestCapture{})
			defer cubeSrv.Close()

			auditStore, _ := store.New(filepath.Join(t.TempDir(), "audit.ndjson"))
			defer auditStore.Close()

			rep := reporter.New(cubeSrv.URL, "", "", false, reporter.WithRetry(1, 10*time.Millisecond))

			var pg admission.PodGetter
			if tc.podInCache {
				pg = &podinformer.Fake{Pods: map[string]*corev1.Pod{
					"cube-system/sandbox-e2e-fo": {ObjectMeta: metav1.ObjectMeta{
						Name:      "sandbox-e2e-fo",
						Namespace: "cube-system",
						Labels:    map[string]string{"cube.master.instance.type": "cubebox"},
					}},
				}}
			} else {
				pg = &podinformer.Fake{Pods: map[string]*corev1.Pod{}}
			}

			handler := admission.New(pg, auditStore, rep)
			tlsSrv := startTLSWebhookServer(t, handler)
			defer tlsSrv.Close()

			review := admissionv1.AdmissionReview{
				Request: &admissionv1.AdmissionRequest{
					UID:       k8stypes.UID(fmt.Sprintf("uid-%s", tc.name)),
					Name:      "sandbox-e2e-fo",
					Namespace: "cube-system",
					UserInfo: authenticationv1.UserInfo{
						Username: "system:node:failopen-test",
					},
				},
			}
			body, _ := json.Marshal(review)
			resp, _ := httpsPost(tlsSrv.URL+"/webhook/eviction", body)
			defer resp.Body.Close()

			respBody, _ := io.ReadAll(resp.Body)
			var result admissionv1.AdmissionReview
			json.Unmarshal(respBody, &result)

			if result.Response != nil && result.Response.Allowed {
				t.Error("eviction must be denied regardless of Pod cache state")
			}
		})
	}
}

// TestE2EHealthz verifies the health endpoint.
func TestE2EHealthz(t *testing.T) {
	handler := admission.New(&podinformer.Fake{}, nil, &nilReporter{})
	tlsSrv := startTLSWebhookServer(t, handler)
	defer tlsSrv.Close()

	resp, err := httpsGet(tlsSrv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for healthz, got %d", resp.StatusCode)
	}
}

// ── Helpers ──

// testServer wraps an http.Server with its URL.
type testServer struct {
	*http.Server
	URL string // "http://host:port" or "https://host:port"
}

type requestCapture struct {
	mu   sync.Mutex
	body []byte
}

func (c *requestCapture) record(body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body = append([]byte{}, body...)
}

func (c *requestCapture) lastBody() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte{}, c.body...)
}

func startFakeCubeMaster(t *testing.T, received *atomic.Int32, capture *requestCapture) *testServer {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/event/eviction", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		b, _ := io.ReadAll(r.Body)
		capture.record(b)
		received.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ret":{"ret_code":200,"ret_msg":"ok"}}`))
	})
	srv := &http.Server{Addr: "127.0.0.1:0", Handler: mux}
	// Bind before returning so the caller gets the correct address
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake CubeMaster listen: %v", err)
	}
	srv.Addr = listener.Addr().String()
	go srv.Serve(listener)
	return &testServer{Server: srv, URL: "http://" + listener.Addr().String()}
}

func startTLSWebhookServer(t *testing.T, admissionHandler http.Handler) *testServer {
	t.Helper()

	certFile, keyFile := writeTestCertKey(t)

	mux := http.NewServeMux()
	mux.Handle("/webhook/eviction", admissionHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tlsCert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("load test TLS: %v", err)
	}

	srv := &http.Server{
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{tlsCert},
			MinVersion:   tls.VersionTLS12,
		},
		Handler: mux,
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("webhook listen: %v", err)
	}
	srv.Addr = listener.Addr().String()

	go srv.ServeTLS(listener, "", "")
	return &testServer{Server: srv, URL: "https://" + listener.Addr().String()}
}

func httpsPost(url string, body []byte) (*http.Response, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	return client.Post(url, "application/json", bytes.NewReader(body))
}

func httpsGet(url string) (*http.Response, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	return client.Get(url)
}

type nilReporter struct{}

func (n *nilReporter) Report(event *types.EvictionEvent) <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

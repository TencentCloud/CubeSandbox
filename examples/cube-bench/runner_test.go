package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunWarmupCompletesBeforeBenchmark(t *testing.T) {
	requestBody, err := buildCreateRequestBody("tpl-warmup", "", networkPolicyNone)
	if err != nil {
		t.Fatalf("buildCreateRequestBody returned error: %v", err)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"sandboxID":"sb-warmup"}`)
	}))
	defer server.Close()

	cfg := &Config{
		Concurrency:    1,
		Warmup:         3,
		Mode:           "create-only",
		APIURL:         server.URL,
		requestBody:    requestBody,
		requestHeaders: map[string]string{},
	}
	var output bytes.Buffer

	client := RunWarmup(cfg, &output)

	if requests != 3 {
		t.Fatalf("requests=%d, want 3", requests)
	}
	if client == nil {
		t.Fatal("RunWarmup returned a nil client")
	}
	want := "    warmup [1/3] ok\n    warmup [2/3] ok\n    warmup [3/3] ok\n\n"
	if output.String() != want {
		t.Fatalf("output=%q, want %q", output.String(), want)
	}
}

func TestBenchOneSendsHostMountMetadata(t *testing.T) {
	rawHostMount := `[
		{"hostPath":"/tmp/data","mountPath":"/mnt/data","readOnly":false}
	]`
	hostMountValue, err := prepareHostMount(rawHostMount)
	if err != nil {
		t.Fatalf("prepareHostMount returned error: %v", err)
	}
	requestBody, err := buildCreateRequestBody("tpl-test", hostMountValue, networkPolicyNone)
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
	requestBody, err := buildCreateRequestBody("tpl-delete", "", networkPolicyNone)
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

// TestRunScheduledEndToEnd drives the scheduled path against a mock CubeAPI:
// requests must arrive in sequence order (concurrency=1), each create body
// must carry the scheduled templateID and timeout=lifetime+60, and each DELETE
// must fire only after the sandbox's lifetime elapsed.
func TestRunScheduledEndToEnd(t *testing.T) {
	type createRec struct {
		body map[string]any
		at   time.Time
	}
	var mu sync.Mutex
	var creates []createRec
	deleteDelay := map[string]time.Duration{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "decode body failed", http.StatusBadRequest)
				return
			}
			mu.Lock()
			id := fmt.Sprintf("sb-%d", len(creates))
			creates = append(creates, createRec{body: body, at: time.Now()})
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"sandboxID":%q}`, id)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/sandboxes/"):
			id := strings.TrimPrefix(r.URL.Path, "/sandboxes/")
			mu.Lock()
			var n int
			if _, err := fmt.Sscanf(id, "sb-%d", &n); err == nil && n < len(creates) {
				deleteDelay[id] = time.Since(creates[n].at)
			}
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	cfg := &Config{
		Concurrency:    1,
		Total:          4,
		Mode:           "create-delete",
		APIURL:         server.URL,
		Seed:           99,
		Rate:           0, // asap; concurrency=1 keeps order deterministic
		hasLifetime:    true,
		LifetimeMin:    0.30,
		LifetimeMax:    0.35,
		Scheduled:      true,
		Templates:      []TemplateSpec{{TemplateID: "tpl-a", Weight: 1}, {TemplateID: "tpl-b", Weight: 1}},
		requestHeaders: map[string]string{},
		NetworkPolicy:  networkPolicyNone,
	}
	sched := GenerateSequence(cfg, rand.New(rand.NewSource(cfg.Seed)))

	resultCh := make(chan IterResult, cfg.Total)
	go RunScheduled(cfg, sched, resultCh, server.Client())
	var results []IterResult
	for r := range resultCh {
		results = append(results, r)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(creates) != cfg.Total {
		t.Fatalf("creates = %d, want %d", len(creates), cfg.Total)
	}
	for i, rec := range creates {
		wantTpl := sched[i].TemplateID
		if got := rec.body["templateID"]; got != wantTpl {
			t.Fatalf("create #%d templateID = %v, want %s (order broken?)", i, got, wantTpl)
		}
		wantTimeout := float64(int64(sched[i].Lifetime.Seconds()) + 60)
		if got, ok := rec.body["timeout"]; !ok || got != wantTimeout {
			t.Fatalf("create #%d timeout = %v (ok=%v), want %v", i, got, ok, wantTimeout)
		}
	}
	if len(deleteDelay) != cfg.Total {
		t.Fatalf("deletes = %d, want %d", len(deleteDelay), cfg.Total)
	}
	for i := range sched {
		id := fmt.Sprintf("sb-%d", i)
		if got := deleteDelay[id]; got < time.Duration(float64(sched[i].Lifetime)*0.9) {
			t.Fatalf("delete %s fired after %v, want >= ~lifetime %v", id, got, sched[i].Lifetime)
		}
	}

	if len(results) != cfg.Total {
		t.Fatalf("results = %d, want %d", len(results), cfg.Total)
	}
	for _, r := range results {
		if r.Err != "" {
			t.Fatalf("result #%d error: %s", r.Seq, r.Err)
		}
		wantTpl := sched[r.Seq].TemplateID
		if r.TemplateID != wantTpl {
			t.Fatalf("result #%d TemplateID = %q, want %q", r.Seq, r.TemplateID, wantTpl)
		}
		wantLifeMs := float64(sched[r.Seq].Lifetime.Microseconds()) / 1000.0
		if r.LifetimeMs != wantLifeMs {
			t.Fatalf("result #%d LifetimeMs = %v, want %v", r.Seq, r.LifetimeMs, wantLifeMs)
		}
		if r.SchedDelayMs < -1 {
			t.Fatalf("result #%d SchedDelayMs = %v, want >= ~0", r.Seq, r.SchedDelayMs)
		}
	}

	// Every request was admitted to the single worker slot exactly once, and
	// the dispatch window was published through the atomic accessor.
	if got := cfg.released.Load(); got != int64(cfg.Total) {
		t.Fatalf("released = %d, want %d", got, cfg.Total)
	}
	if got := cfg.getDispatchElapsed(); got <= 0 {
		t.Fatalf("dispatchElapsed = %v, want > 0", got)
	}
}

// TestRunScheduledDryRunReproducible verifies the dry-run scheduled path uses
// a derived rng: same seed -> identical results, and lifetimes are honored.
func TestRunScheduledDryRunReproducible(t *testing.T) {
	newCfg := func() *Config {
		return &Config{
			Concurrency:    3,
			Total:          12,
			Mode:           "create-delete",
			DryRun:         true,
			DryLatencyMean: 2,
			DryLatencyStd:  1,
			DryErrorRate:   0,
			Seed:           5,
			Rate:           0,
			hasLifetime:    true,
			LifetimeMin:    0.01,
			LifetimeMax:    0.02,
			Scheduled:      true,
			Templates:      []TemplateSpec{{TemplateID: "tpl-dry", Weight: 1}},
		}
	}
	run := func() map[int]IterResult {
		cfg := newCfg()
		sched := GenerateSequence(cfg, rand.New(rand.NewSource(cfg.Seed)))
		resultCh := make(chan IterResult, cfg.Total)
		go RunScheduled(cfg, sched, resultCh, nil)
		results := map[int]IterResult{}
		for r := range resultCh {
			results[r.Seq] = r
		}
		return results
	}

	a := run()
	b := run()
	if len(a) != 12 {
		t.Fatalf("results = %d, want 12", len(a))
	}
	for seq := 0; seq < 12; seq++ {
		ra, okA := a[seq]
		rb, okB := b[seq]
		if !okA || !okB {
			t.Fatalf("seq %d missing (a=%v b=%v)", seq, okA, okB)
		}
		if ra.Err != "" {
			t.Fatalf("unexpected error: %s", ra.Err)
		}
		if ra.CreateMs != rb.CreateMs || ra.DeleteMs != rb.DeleteMs {
			t.Fatalf("seq %d not reproducible: %v/%v vs %v/%v",
				seq, ra.CreateMs, ra.DeleteMs, rb.CreateMs, rb.DeleteMs)
		}
		if ra.TemplateID != "tpl-dry" {
			t.Fatalf("seq %d TemplateID = %q", seq, ra.TemplateID)
		}
		if ra.LifetimeMs < 10 || ra.LifetimeMs > 20 {
			t.Fatalf("seq %d LifetimeMs = %v, want within [10, 20]", seq, ra.LifetimeMs)
		}
	}
}

// TestBenchOneLegacyLeavesScheduledFieldsZero pins legacy behavior: without
// scheduling flags the new IterResult diagnostics stay zero.
func TestBenchOneLegacyLeavesScheduledFieldsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"sandboxID":"sb-legacy"}`)
	}))
	defer server.Close()

	cfg := &Config{
		Mode:           "create-only",
		APIURL:         server.URL,
		requestBody:    []byte(`{"templateID":"tpl-legacy"}`),
		requestHeaders: map[string]string{},
	}
	r := benchOne(server.Client(), cfg, 1)
	if r.Err != "" {
		t.Fatalf("benchOne error: %s", r.Err)
	}
	if r.TemplateID != "" || r.ScheduledArrivalMs != 0 || r.ActualStartMs != 0 || r.SchedDelayMs != 0 || r.LifetimeMs != 0 {
		t.Fatalf("legacy result has scheduled fields set: %+v", r)
	}
}

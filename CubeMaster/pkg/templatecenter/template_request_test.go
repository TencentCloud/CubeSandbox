package templatecenter

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestEffectiveArtifactDownloadBaseURLPrefersConfiguredMasterAddr(t *testing.T) {
	t.Setenv("CUBE_MASTER_ADDR", "http://cube-master.cube-system.svc:8089")
	artifact := &models.RootfsArtifact{MasterNodeIP: "http://0.0.0.0:8089"}
	if got, want := effectiveArtifactDownloadBaseURL("http://fallback.example", artifact), "http://cube-master.cube-system.svc:8089"; got != want {
		t.Fatalf("effectiveArtifactDownloadBaseURL() = %q, want %q", got, want)
	}
}

func TestEffectiveArtifactDownloadBaseURLRewritesLoopbackEnvWithSharedNodeIP(t *testing.T) {
	t.Setenv("CUBE_MASTER_ADDR", "http://127.0.0.1:8089")
	t.Setenv("CUBE_SANDBOX_NODE_IP", "10.0.0.8")
	artifact := &models.RootfsArtifact{MasterNodeIP: "http://master-from-row:8089"}
	if got, want := effectiveArtifactDownloadBaseURL("http://fallback.example", artifact), "http://10.0.0.8:8089"; got != want {
		t.Fatalf("effectiveArtifactDownloadBaseURL() = %q, want %q", got, want)
	}
}

func TestEffectiveArtifactDownloadBaseURLSkipsUnrewritableLoopbackEnv(t *testing.T) {
	t.Setenv("CUBE_MASTER_ADDR", "http://127.0.0.1:8089")
	artifact := &models.RootfsArtifact{MasterNodeIP: "http://master-from-row:8089"}
	if got, want := effectiveArtifactDownloadBaseURL("http://fallback.example", artifact), "http://fallback.example"; got != want {
		t.Fatalf("effectiveArtifactDownloadBaseURL() = %q, want %q", got, want)
	}
}

func TestEffectiveArtifactDownloadBaseURLFallsBackToArtifactRow(t *testing.T) {
	artifact := &models.RootfsArtifact{MasterNodeIP: "http://master-from-row:8089"}
	if got, want := effectiveArtifactDownloadBaseURL("", artifact), "http://master-from-row:8089"; got != want {
		t.Fatalf("effectiveArtifactDownloadBaseURL() = %q, want %q", got, want)
	}
}

func TestEffectiveArtifactDownloadBaseURLRewritesLoopbackArtifactRowWithSharedNodeIP(t *testing.T) {
	t.Setenv("CUBE_SANDBOX_NODE_IP", "10.0.0.8")
	artifact := &models.RootfsArtifact{MasterNodeIP: "http://127.0.0.1:8089"}
	if got, want := effectiveArtifactDownloadBaseURL("", artifact), "http://10.0.0.8:8089"; got != want {
		t.Fatalf("effectiveArtifactDownloadBaseURL() = %q, want %q", got, want)
	}
}

func TestCloneEgressRuleDeepCopiesPort(t *testing.T) {
	port := 8443
	rule := &types.EgressRule{
		Name: "custom-https",
		Match: &types.EgressRuleMatch{
			Port: &port,
		},
	}

	cloned := rule.DeepCopy()
	if cloned == nil || cloned.Match == nil || cloned.Match.Port == nil {
		t.Fatalf("cloned rule lost port: %+v", cloned)
	}
	if *cloned.Match.Port != port {
		t.Fatalf("cloned port=%d, want %d", *cloned.Match.Port, port)
	}
	if cloned.Match.Port == rule.Match.Port {
		t.Fatal("cloned port aliases source pointer")
	}

	*cloned.Match.Port = 443
	if *rule.Match.Port != 8443 {
		t.Fatalf("source port changed through clone: %d", *rule.Match.Port)
	}
}

type spanRecorder struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (r *spanRecorder) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, spans...)
	return nil
}

func (r *spanRecorder) Shutdown(context.Context) error { return nil }

func (r *spanRecorder) snapshot() []sdktrace.ReadOnlySpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sdktrace.ReadOnlySpan, len(r.spans))
	copy(out, r.spans)
	return out
}

func installSpanRecorder(t *testing.T) (*spanRecorder, func()) {
	t.Helper()
	prev := otel.GetTracerProvider()
	rec := &spanRecorder{}
	shutdown, err := telemetry.SetupWithExporter(rec, "cubemaster-test")
	if err != nil {
		t.Fatalf("SetupWithExporter: %v", err)
	}
	var once sync.Once
	flush := func() {
		once.Do(func() {
			if err := shutdown(context.Background()); err != nil {
				t.Errorf("shutdown: %v", err)
			}
		})
	}
	t.Cleanup(func() {
		flush()
		otel.SetTracerProvider(prev)
	})
	return rec, flush
}

func TestEndSnapshotReadSpanTreatsMissAsSuccess(t *testing.T) {
	rec, flush := installSpanRecorder(t)

	_, span := telemetry.Start(context.Background(), telemetry.SpanTemplateDB)
	endSnapshotReadSpan(span, ErrSnapshotNotFound)
	flush()

	spans := rec.snapshot()
	if len(spans) != 1 {
		t.Fatalf("got %d span(s), want 1", len(spans))
	}
	if got := spans[0].Status().Code; got == codes.Error {
		t.Errorf("expected snapshot miss marked the read as %v, want a non-error status", got)
	}
}

func TestEndSnapshotReadSpanKeepsRealFailures(t *testing.T) {
	rec, flush := installSpanRecorder(t)

	_, span := telemetry.Start(context.Background(), telemetry.SpanTemplateDB)
	endSnapshotReadSpan(span, errors.New("connection refused"))
	flush()

	spans := rec.snapshot()
	if len(spans) != 1 {
		t.Fatalf("got %d span(s), want 1", len(spans))
	}
	if got := spans[0].Status().Code; got != codes.Error {
		t.Errorf("real query failure status = %v, want %v", got, codes.Error)
	}
}

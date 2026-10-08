// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubelet

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	grpcpool "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/grpc-middleware/pool"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/telemetry"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet/grpcconn"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	errorcodev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

type captureServer struct {
	cubeboxv1.UnimplementedCubeboxMgrServer
	traceparent chan string
}

func (s *captureServer) CommitSandbox(ctx context.Context, _ *cubeboxv1.CommitSandboxRequest) (*cubeboxv1.CommitSandboxResponse, error) {
	tp := ""
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if values := md.Get("traceparent"); len(values) > 0 {
			tp = values[0]
		}
	}
	s.traceparent <- tp
	return &cubeboxv1.CommitSandboxResponse{Ret: &errorcodev1.Ret{RetCode: errorcodev1.ErrorCode_Success}}, nil
}

type fakeConn struct{ cc *grpc.ClientConn }

func (c fakeConn) Value() *grpc.ClientConn { return c.cc }
func (c fakeConn) Close() error            { return nil }

func TestCommitSandboxPropagatesTraceToCubelet(t *testing.T) {
	prevProvider := otel.GetTracerProvider()
	prevPropagator := otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevProvider)
		otel.SetTextMapPropagator(prevPropagator)
	})

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	server := &captureServer{traceparent: make(chan string, 1)}
	cubeboxv1.RegisterCubeboxMgrServer(srv, server)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	patches := gomonkey.NewPatches()
	t.Cleanup(patches.Reset)
	patches.ApplyFunc(grpcconn.GetWorkerConn, func(context.Context, string) (grpcpool.Conn, error) {
		return fakeConn{cc: cc}, nil
	})

	ctx, span := telemetry.Start(context.Background(), "test.root")
	defer span.End()

	if _, err := CommitSandbox(ctx, "cubelet.invalid:5000", &cubeboxv1.CommitSandboxRequest{}); err != nil {
		t.Fatalf("CommitSandbox: %v", err)
	}

	select {
	case tp := <-server.traceparent:
		parts := strings.Split(tp, "-")
		if len(parts) != 4 {
			t.Fatalf("traceparent %q is malformed", tp)
		}
		if parts[1] != span.SpanContext().TraceID().String() {
			t.Errorf("trace id = %s, want %s", parts[1], span.SpanContext().TraceID())
		}
		if parts[2] != span.SpanContext().SpanID().String() {
			t.Errorf("parent span id = %s, want %s", parts[2], span.SpanContext().SpanID())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cubelet never received the commit RPC")
	}
}

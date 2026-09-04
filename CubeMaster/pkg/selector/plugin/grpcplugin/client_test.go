// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package grpcplugin

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	schedulerplugin "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/schedulerplugin/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/config"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/scheduler/selctx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type fakeSchedulerPlugin struct {
	schedulerplugin.UnimplementedSchedulerPluginServer
	mu            sync.Mutex
	lastSnapshot  string
	invalidFilter bool
	invalidScore  bool
	failFilter    bool
}

func (f *fakeSchedulerPlugin) Handshake(context.Context, *schedulerplugin.HandshakeRequest) (*schedulerplugin.HandshakeResponse, error) {
	return &schedulerplugin.HandshakeResponse{ProtocolVersion: ProtocolVersion, PluginName: "fake", Capabilities: []string{"filter", "score"}}, nil
}

func (f *fakeSchedulerPlugin) SyncSnapshot(_ context.Context, request *schedulerplugin.SnapshotRequest) (*schedulerplugin.SnapshotResponse, error) {
	f.mu.Lock()
	f.lastSnapshot = request.GetSnapshotVersion()
	f.mu.Unlock()
	return &schedulerplugin.SnapshotResponse{SnapshotVersion: request.GetSnapshotVersion()}, nil
}

func (f *fakeSchedulerPlugin) Filter(_ context.Context, request *schedulerplugin.FilterRequest) (*schedulerplugin.FilterResponse, error) {
	f.mu.Lock()
	invalid := f.invalidFilter
	fail := f.failFilter
	f.mu.Unlock()
	if fail {
		return nil, status.Error(codes.Unavailable, "filter unavailable")
	}
	if invalid {
		return &schedulerplugin.FilterResponse{SnapshotVersion: request.GetSnapshotVersion(), KeptIds: []string{"not-a-candidate"}}, nil
	}
	return &schedulerplugin.FilterResponse{SnapshotVersion: request.GetSnapshotVersion(), KeptIds: []string{request.GetCandidateIds()[0]}}, nil
}

func (f *fakeSchedulerPlugin) Score(_ context.Context, request *schedulerplugin.ScoreRequest) (*schedulerplugin.ScoreResponse, error) {
	f.mu.Lock()
	invalid := f.invalidScore
	f.mu.Unlock()
	response := &schedulerplugin.ScoreResponse{SnapshotVersion: request.GetSnapshotVersion()}
	for index, id := range request.GetCandidateIds() {
		value := float64(90 - index*10)
		if invalid && index == 0 {
			value = 101
		}
		response.Scores = append(response.Scores, &schedulerplugin.NodeScore{NodeId: id, Score: value})
	}
	return response, nil
}

func startPluginServer(t *testing.T) (*grpc.ClientConn, *fakeSchedulerPlugin) {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	fake := &fakeSchedulerPlugin{}
	schedulerplugin.RegisterSchedulerPluginServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	connection, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	return connection, fake
}

func grpcSelection() *selctx.SelectorCtx {
	selection := selctx.New("random")
	selection.Ctx = context.Background()
	selection.InstanceType = "small"
	selection.SetNodes(node.NodeList{
		{InsID: "n1", Healthy: true, CpuUtil: 10},
		{InsID: "n2", Healthy: true, CpuUtil: 20},
	})
	selection.FreezeSnapshot()
	return selection
}

func TestExternalFilterSynchronizesAndValidatesSnapshot(t *testing.T) {
	connection, server := startPluginServer(t)
	client, err := newClientFromConn(context.Background(), config.SchedulerProfilePluginConf{
		Name: "fake", Timeout: time.Second,
	}, "filter", connection)
	if err != nil {
		t.Fatal(err)
	}
	selector := &filterPlugin{client: client}
	t.Cleanup(func() { _ = selector.Close() })
	selection := grpcSelection()
	kept, err := selector.Select(selection)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].ID() != "n1" {
		t.Fatalf("kept = %v", kept)
	}
	server.mu.Lock()
	lastSnapshot := server.lastSnapshot
	server.invalidFilter = true
	server.mu.Unlock()
	if lastSnapshot != selection.SnapshotVersion {
		t.Fatalf("synced version = %q, want %q", lastSnapshot, selection.SnapshotVersion)
	}
	selection.FreezeSnapshot()
	if _, err := selector.Select(selection); err == nil {
		t.Fatal("non-candidate filter result must be rejected")
	}
}

func TestExternalScoreRejectsOutOfRangeValues(t *testing.T) {
	connection, server := startPluginServer(t)
	client, err := newClientFromConn(context.Background(), config.SchedulerProfilePluginConf{
		Name: "fake", Timeout: time.Second, Weight: 2,
	}, "score", connection)
	if err != nil {
		t.Fatal(err)
	}
	selector := &scorePlugin{client: client, weight: 2}
	t.Cleanup(func() { _ = selector.Close() })
	scores, err := selector.Select(grpcSelection())
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 2 || scores[0].Score != 90 || scores[1].Score != 80 {
		t.Fatalf("scores = %+v", scores)
	}
	server.mu.Lock()
	server.invalidScore = true
	server.mu.Unlock()
	if _, err := selector.Select(grpcSelection()); err == nil {
		t.Fatal("out-of-range external score must be rejected")
	}
}

func TestExternalFilterCircuitBreakerCountsFailuresAcrossSnapshotSync(t *testing.T) {
	connection, server := startPluginServer(t)
	client, err := newClientFromConn(context.Background(), config.SchedulerProfilePluginConf{
		Name: "fake", Timeout: time.Second, CircuitBreakerFailures: 2, CircuitBreakerCooldown: time.Minute,
	}, "filter", connection)
	if err != nil {
		t.Fatal(err)
	}
	selector := &filterPlugin{client: client}
	t.Cleanup(func() { _ = selector.Close() })
	server.mu.Lock()
	server.failFilter = true
	server.mu.Unlock()

	for attempt := 0; attempt < 2; attempt++ {
		if _, err := selector.Select(grpcSelection()); err == nil || errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("attempt %d error = %v, want RPC failure", attempt+1, err)
		}
	}
	if _, err := selector.Select(grpcSelection()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("third attempt error = %v, want circuit open", err)
	}
}

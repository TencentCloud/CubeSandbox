// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// scheduler-plugin is a minimal external Filter+Score plugin example. It keeps
// nodes with fewer than eight in-flight creates and scores lower CPU usage
// higher. Run with SOCKET=/run/cube-scheduler-example.sock.
//
// The server is stateless: every Filter/Score request carries the frozen
// candidate snapshot for its snapshot_version, so concurrent scheduling
// attempts never contend on shared server-side state.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	schedulerplugin "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/schedulerplugin/v1"
	"google.golang.org/grpc"
)

const protocolVersion = "v1"

type server struct {
	schedulerplugin.UnimplementedSchedulerPluginServer
}

func (s *server) Handshake(_ context.Context, request *schedulerplugin.HandshakeRequest) (*schedulerplugin.HandshakeResponse, error) {
	if request.GetProtocolVersion() != protocolVersion {
		return nil, fmt.Errorf("unsupported protocol version %q", request.GetProtocolVersion())
	}
	return &schedulerplugin.HandshakeResponse{
		ProtocolVersion: protocolVersion,
		PluginName:      request.GetPluginName(),
		Capabilities:    []string{"filter", "score"},
	}, nil
}

func snapshotIndex(snapshot []*schedulerplugin.SnapshotNode) map[string]*schedulerplugin.SnapshotNode {
	nodes := make(map[string]*schedulerplugin.SnapshotNode, len(snapshot))
	for _, candidate := range snapshot {
		nodes[candidate.GetId()] = candidate
	}
	return nodes
}

func (s *server) Filter(_ context.Context, request *schedulerplugin.FilterRequest) (*schedulerplugin.FilterResponse, error) {
	nodes := snapshotIndex(request.GetSnapshot())
	response := &schedulerplugin.FilterResponse{SnapshotVersion: request.GetSnapshotVersion()}
	for _, id := range request.GetCandidateIds() {
		candidate := nodes[id]
		if candidate != nil && candidate.GetCreating() < 8 {
			response.KeptIds = append(response.KeptIds, id)
		}
	}
	return response, nil
}

func (s *server) Score(_ context.Context, request *schedulerplugin.ScoreRequest) (*schedulerplugin.ScoreResponse, error) {
	nodes := snapshotIndex(request.GetSnapshot())
	response := &schedulerplugin.ScoreResponse{SnapshotVersion: request.GetSnapshotVersion()}
	for _, id := range request.GetCandidateIds() {
		candidate := nodes[id]
		if candidate == nil {
			return nil, fmt.Errorf("unknown candidate %q", id)
		}
		value := 100 - candidate.GetCpuUtil()
		if value < 0 {
			value = 0
		}
		if value > 100 {
			value = 100
		}
		response.Scores = append(response.Scores, &schedulerplugin.NodeScore{NodeId: id, Score: value})
	}
	return response, nil
}

func main() {
	socket := os.Getenv("SOCKET")
	if socket == "" {
		socket = "/run/cube-scheduler-example.sock"
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		log.Fatalf("listen on %s: %v", socket, err)
	}
	grpcServer := grpc.NewServer()
	schedulerplugin.RegisterSchedulerPluginServer(grpcServer, &server{})
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		grpcServer.GracefulStop()
	}()
	log.Printf("scheduler plugin listening on %s", socket)
	if err := grpcServer.Serve(listener); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

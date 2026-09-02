// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// scheduler-plugin is a minimal external Filter+Score plugin example. It keeps
// nodes with fewer than eight in-flight creates and scores lower CPU usage
// higher. Run with SOCKET=/run/cube-scheduler-example.sock.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	schedulerplugin "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/schedulerplugin/v1"
	"google.golang.org/grpc"
)

const protocolVersion = "v1"

type server struct {
	schedulerplugin.UnimplementedSchedulerPluginServer
	mu      sync.RWMutex
	version string
	nodes   map[string]*schedulerplugin.SnapshotNode
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

func (s *server) SyncSnapshot(_ context.Context, request *schedulerplugin.SnapshotRequest) (*schedulerplugin.SnapshotResponse, error) {
	nodes := make(map[string]*schedulerplugin.SnapshotNode, len(request.GetNodes()))
	for _, candidate := range request.GetNodes() {
		nodes[candidate.GetId()] = candidate
	}
	s.mu.Lock()
	s.version = request.GetSnapshotVersion()
	s.nodes = nodes
	s.mu.Unlock()
	return &schedulerplugin.SnapshotResponse{SnapshotVersion: request.GetSnapshotVersion()}, nil
}

func (s *server) Filter(_ context.Context, request *schedulerplugin.FilterRequest) (*schedulerplugin.FilterResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if request.GetSnapshotVersion() != s.version {
		return nil, fmt.Errorf("snapshot %q is not synchronized", request.GetSnapshotVersion())
	}
	response := &schedulerplugin.FilterResponse{SnapshotVersion: s.version}
	for _, id := range request.GetCandidateIds() {
		candidate := s.nodes[id]
		if candidate != nil && candidate.GetCreating() < 8 {
			response.KeptIds = append(response.KeptIds, id)
		}
	}
	return response, nil
}

func (s *server) Score(_ context.Context, request *schedulerplugin.ScoreRequest) (*schedulerplugin.ScoreResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if request.GetSnapshotVersion() != s.version {
		return nil, fmt.Errorf("snapshot %q is not synchronized", request.GetSnapshotVersion())
	}
	response := &schedulerplugin.ScoreResponse{SnapshotVersion: s.version}
	for _, id := range request.GetCandidateIds() {
		candidate := s.nodes[id]
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
	// A previous unclean exit can leave the socket file behind; remove it so
	// the example restarts without manual cleanup.
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Fatalf("remove stale socket %s: %v", socket, err)
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

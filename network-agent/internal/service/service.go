// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package service

import "context"

// Service is the minimal network-agent runtime interface.
type Service interface {
	EnsureNetwork(ctx context.Context, req *EnsureNetworkRequest) (*EnsureNetworkResponse, error)
	ReleaseNetwork(ctx context.Context, req *ReleaseNetworkRequest) (*ReleaseNetworkResponse, error)
	ReconcileNetwork(ctx context.Context, req *ReconcileNetworkRequest) (*ReconcileNetworkResponse, error)
	GetNetwork(ctx context.Context, req *GetNetworkRequest) (*GetNetworkResponse, error)
	ListNetworks(ctx context.Context, req *ListNetworksRequest) (*ListNetworksResponse, error)
	Health(ctx context.Context) error

	// DumpEgressPolicies returns every active sandbox's L7 egress
	// policy in the JSON shape CubeEgress's bootstrap.lua expects.
	// Used by GET /v1/policies/dump (CUBE_EGRESS_BOOTSTRAP_URL points
	// at this endpoint). Sandboxes without rules are omitted, so an
	// empty map is the correct response when no L7 policy is in play.
	//
	// Returns marshal-ready map: keys are sandbox IPs, values are the
	// `{policy_id, rules: [...]}` body that PUT /admin/v1/policies/<ip>
	// would carry — guaranteeing the per-sandbox push and the bulk
	// dump never disagree about how a rule is encoded.
	DumpEgressPolicies(ctx context.Context) (map[string]map[string]any, error)
}

type noopService struct{}

// NewNoopService returns the default placeholder service implementation.
func NewNoopService() Service {
	return &noopService{}
}

func (s *noopService) EnsureNetwork(ctx context.Context, req *EnsureNetworkRequest) (*EnsureNetworkResponse, error) {
	return &EnsureNetworkResponse{
		SandboxID:       req.SandboxID,
		NetworkHandle:   req.SandboxID,
		PersistMetadata: req.PersistMetadata,
	}, nil
}

func (s *noopService) ReleaseNetwork(ctx context.Context, req *ReleaseNetworkRequest) (*ReleaseNetworkResponse, error) {
	return &ReleaseNetworkResponse{
		Released:        true,
		PersistMetadata: req.PersistMetadata,
	}, nil
}

func (s *noopService) ReconcileNetwork(ctx context.Context, req *ReconcileNetworkRequest) (*ReconcileNetworkResponse, error) {
	return &ReconcileNetworkResponse{
		NetworkHandle:   req.NetworkHandle,
		Converged:       true,
		PersistMetadata: req.PersistMetadata,
	}, nil
}

func (s *noopService) GetNetwork(ctx context.Context, req *GetNetworkRequest) (*GetNetworkResponse, error) {
	return &GetNetworkResponse{
		SandboxID:     req.SandboxID,
		NetworkHandle: req.NetworkHandle,
	}, nil
}

func (s *noopService) ListNetworks(ctx context.Context, req *ListNetworksRequest) (*ListNetworksResponse, error) {
	return &ListNetworksResponse{}, nil
}

func (s *noopService) Health(ctx context.Context) error {
	return nil
}

func (s *noopService) DumpEgressPolicies(ctx context.Context) (map[string]map[string]any, error) {
	return map[string]map[string]any{}, nil
}

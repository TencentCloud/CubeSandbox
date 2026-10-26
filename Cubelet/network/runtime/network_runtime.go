// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtime

import (
	"context"
	"errors"
	"os"
)

// ErrEnsureNetworkCommitted marks an EnsureNetwork error raised after the
// durable success point. The plugin must run the normal ReleaseNetwork path
// before returning the create failure; pre-commit errors must not do so because
// another retry may already have started after EnsureNetwork released its lock.
var ErrEnsureNetworkCommitted = errors.New("network ensure success is already committed")

// ErrNetworkNotActive reports that a sandbox has no active network, so its
// policy cannot be updated. Distinct from a generic failure because callers
// surface it as a client-side conflict rather than a server error.
var ErrNetworkNotActive = errors.New("sandbox network is not active")

// NetworkRuntime is the in-process network runtime interface used by Cubelet.
// Implementations must make EnsureNetwork and ReleaseNetwork idempotent for the
// same sandbox so Cubelet can safely retry after process or RPC failures.
type NetworkRuntime interface {
	// EnsureNetwork creates or returns the already-created network for a sandbox.
	EnsureNetwork(ctx context.Context, req *EnsureNetworkRequest) (*EnsureNetworkResponse, error)
	// ReleaseNetwork marks a sandbox network for cleanup and returns once cleanup
	// ownership has been handed to the runtime, not necessarily after every kernel
	// side effect has completed.
	ReleaseNetwork(ctx context.Context, req *ReleaseNetworkRequest) (*ReleaseNetworkResponse, error)
	// UpdateNetworkPolicy replaces the egress policy of a running sandbox. It
	// returns ErrNetworkNotActive when the sandbox has no active network, which
	// callers map to a "not running" client error.
	UpdateNetworkPolicy(ctx context.Context, req *UpdateNetworkPolicyRequest) error
	// ListTaps returns the TAP pool state machine snapshot used by diagnostics.
	ListTaps(ctx context.Context, req *ListTapsRequest) (*ListTapsResponse, error)
	// Health reports whether the runtime process can still serve requests.
	Health(ctx context.Context) error
	// GetTapFile returns a caller-owned live TAP fd for the sandbox handoff path.
	GetTapFile(sandboxID, tapName string) (*os.File, error)

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

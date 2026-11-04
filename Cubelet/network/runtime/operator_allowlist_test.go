package runtime

import (
	"context"
	"net"
	"slices"
	"testing"
)

func TestOperatorAllowlistLifecycle(t *testing.T) {
	c := newCreateTestController(t, nil)
	c.cfg.OperatorAllowOut = []string{"10.204.0.10/32", "10.20.0.0/24"}
	block := false
	caller := &CubeNetworkConfig{AllowInternetAccess: &block, AllowOut: []string{"api.example.com"}}
	// Both first installation and recovery replacement must layer the same grant.
	if err := c.registerCubeVSTap(77, net.ParseIP("10.30.0.7"), "sb-platform", caller); err != nil {
		t.Fatal(err)
	}
	if err := c.replaceCubeVSTap(77, net.ParseIP("10.30.0.7"), "sb-platform", caller); err != nil {
		t.Fatal(err)
	}
	for _, installed := range c.cubevsAdapter.(*fakeCubeVSAdapter).installedPolicies {
		if installed.AllowOut == nil || !slices.Contains(*installed.AllowOut, "10.204.0.10/32") || !slices.Contains(*installed.AllowOut, "10.20.0.0/24") {
			t.Fatal("create/recovery lost platform grants", installed.AllowOut)
		}
	}
	state := registerActiveSandbox(t, c, "sb-platform", caller, []string{"10.204.0.10/32"})
	for _, targets := range [][]string{{"203.0.113.0/24"}, {"api.example.com"}, nil} {
		if err := c.UpdateNetworkPolicy(context.Background(), &UpdateNetworkPolicyRequest{SandboxID: "sb-platform", CubeNetworkConfig: &CubeNetworkConfig{AllowInternetAccess: &block, AllowOut: targets}}); err != nil {
			t.Fatal(err)
		}
		updates := c.cubevsAdapter.(*fakeCubeVSAdapter).updatedPolicies
		actual := *updates[len(updates)-1].opts.AllowOut
		for _, cidr := range c.cfg.OperatorAllowOut {
			if !slices.Contains(actual, cidr) {
				t.Fatalf("platform grant missing after update: %v", actual)
			}
		}
		if slices.Contains(state.CubeNetworkConfig.AllowOut, "10.20.0.0/24") {
			t.Fatal("platform grant leaked into persisted caller policy")
		}
	}
	// Revocation on restart/recovery must not replay a formerly configured grant.
	c.cfg.OperatorAllowOut = nil
	if err := c.updateCubeVSTapPolicy(77, "sb-platform", state.CubeNetworkConfig); err != nil {
		t.Fatal(err)
	}
	updates := c.cubevsAdapter.(*fakeCubeVSAdapter).updatedPolicies
	actual := updates[len(updates)-1].opts.AllowOut
	if actual != nil && slices.Contains(*actual, "10.20.0.0/24") {
		t.Fatal("revoked platform CIDR survived")
	}
	if len(caller.AllowOut) != 1 {
		t.Fatal("caller policy mutated")
	}
}

func TestOperatorAllowlistEmptyAndNilPolicy(t *testing.T) {
	if withOperatorAllowOut(nil, nil) != nil {
		t.Fatal("empty deployment must preserve nil policy")
	}
	got := withOperatorAllowOut(nil, []string{"10.204.0.10/32"})
	if !slices.Contains(got.AllowOut, "10.204.0.10/32") {
		t.Fatal(got)
	}
}

package runtime

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeNet/cubevs"
	"github.com/tencentcloud/CubeSandbox/Cubelet/network/runtime/systemnet"
)

func TestNetworkRuntimeConcurrentEnsureSameSandboxIsIdempotent(t *testing.T) {
	controller := newCreateTestController(t, nil)
	ctx := context.Background()
	const goroutines = 8

	// All goroutines race the same SandboxID. The controller should create one TAP
	// and return the same network response to every caller.
	var wg sync.WaitGroup
	responses := make([]*EnsureNetworkResponse, goroutines)
	errs := make([]error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			responses[index], errs[index] = controller.EnsureNetwork(ctx, &EnsureNetworkRequest{SandboxID: "sandbox-concurrent"})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("EnsureNetwork[%d] returned error: %v", i, err)
		}
	}
	first := responses[0]
	if first == nil {
		t.Fatal("first response is nil")
	}
	for i, resp := range responses[1:] {
		if resp == nil || resp.NetworkHandle != first.NetworkHandle || resp.Interfaces[0].Name != first.Interfaces[0].Name {
			t.Fatalf("response[%d]=%#v, want same network as %#v", i+1, resp, first)
		}
	}
	taps, err := controller.ListTaps(ctx, &ListTapsRequest{})
	if err != nil {
		t.Fatalf("ListTaps returned error: %v", err)
	}
	if len(taps.Taps) != 1 || taps.Taps[0].SandboxID != "sandbox-concurrent" || taps.Taps[0].State != string(TapPoolActive) {
		t.Fatalf("ListTaps=%#v, want exactly one Active network for sandbox-concurrent", taps.Taps)
	}
	if got := len(controller.tapPool.Entries()); got != 1 {
		t.Fatalf("tap pool entries=%d, want exactly one tap", got)
	}
}

func TestNetworkRuntimeConcurrentGetTapFileAndReleaseAreSerialized(t *testing.T) {
	controller := newCreateTestController(t, nil)
	resp, err := controller.EnsureNetwork(context.Background(), &EnsureNetworkRequest{SandboxID: "sandbox-fd-release-race"})
	if err != nil {
		t.Fatal(err)
	}
	tapName := resp.Interfaces[0].Name

	start := make(chan struct{})
	getDone := make(chan error, 1)
	releaseDone := make(chan error, 1)
	go func() {
		<-start
		file, getErr := controller.GetTapFile("sandbox-fd-release-race", tapName)
		if file != nil {
			_ = file.Close()
		}
		getDone <- getErr
	}()
	go func() {
		<-start
		_, releaseErr := controller.ReleaseNetwork(context.Background(), &ReleaseNetworkRequest{SandboxID: "sandbox-fd-release-race"})
		releaseDone <- releaseErr
	}()
	close(start)

	getErr := <-getDone
	if releaseErr := <-releaseDone; releaseErr != nil {
		t.Fatalf("ReleaseNetwork failed: %v", releaseErr)
	}
	// Depending on lock acquisition order, the handoff either completes before
	// release or observes that release already removed Active ownership.
	if getErr != nil && !strings.Contains(getErr.Error(), "not found") {
		t.Fatalf("GetTapFile returned unexpected concurrent release error: %v", getErr)
	}
	if _, err := controller.GetTapFile("sandbox-fd-release-race", tapName); err == nil {
		t.Fatal("GetTapFile succeeded after release completed")
	}
}

func TestNetworkRuntimeCreateReleaseCleanReuseIntegration(t *testing.T) {
	controller := newCreateTestController(t, nil)
	ctx := context.Background()

	// Full lifecycle: create a sandbox with L7 policy and a generated host port,
	// release it synchronously, then create a second sandbox that reuses the
	// cleaned TAP and released host port.
	created, err := controller.EnsureNetwork(ctx, &EnsureNetworkRequest{
		SandboxID: "sandbox-integration-a",
		PortMappings: []PortMapping{{
			ContainerPort: 8080,
		}},
		CubeNetworkConfig: &CubeNetworkConfig{Rules: []*EgressRule{{
			Name:   "allow-api",
			Action: &EgressRuleAction{Allow: true},
		}}},
	})
	if err != nil {
		t.Fatalf("EnsureNetwork returned error: %v", err)
	}
	if len(created.PortMappings) != 1 || created.PortMappings[0].HostPort == 0 {
		t.Fatalf("created port mappings = %#v", created.PortMappings)
	}
	hostPort := created.PortMappings[0].HostPort
	tapName := created.Interfaces[0].Name
	file, err := controller.GetTapFile("sandbox-integration-a", tapName)
	if err != nil {
		t.Fatalf("GetTapFile after create: %v", err)
	}
	_ = file.Close()
	taps, err := controller.ListTaps(ctx, &ListTapsRequest{})
	if err != nil {
		t.Fatalf("ListTaps after create returned error: %v", err)
	}
	if len(taps.Taps) != 1 || taps.Taps[0].SandboxID != "sandbox-integration-a" || taps.Taps[0].State != string(TapPoolActive) {
		t.Fatalf("ListTaps after create = %#v, want one Active sandbox-integration-a", taps.Taps)
	}

	if _, err := controller.ReleaseNetwork(ctx, &ReleaseNetworkRequest{SandboxID: "sandbox-integration-a"}); err != nil {
		t.Fatalf("ReleaseNetwork returned error: %v", err)
	}
	if _, err := controller.GetTapFile("sandbox-integration-a", tapName); err == nil {
		t.Fatal("GetTapFile succeeded after release accepted; want unavailable")
	}
	if controller.store.Exists("sandbox-integration-a", StateFileDeleting) {
		t.Fatal("deleting state remains after successful release")
	}
	taps, err = controller.ListTaps(ctx, &ListTapsRequest{})
	if err != nil {
		t.Fatalf("ListTaps after release returned error: %v", err)
	}
	if len(taps.Taps) != 1 || taps.Taps[0].State != string(TapPoolReady) || taps.Taps[0].OwnerSandboxID != "" {
		t.Fatalf("ListTaps after release=%#v, want Ready free tap", taps.Taps)
	}
	if taps.StateCounts[string(TapPoolReady)] != 1 {
		t.Fatalf("ListTaps stateCounts after release=%v, want one Ready", taps.StateCounts)
	}
	if controller.store.Exists("sandbox-integration-a", StateFileSuccess) {
		t.Fatal("state file still exists after successful cleanup")
	}
	poolState, owner, ok := controller.tapPool.StateByName(tapName)
	if !ok || poolState != TapPoolReady || owner != "" {
		t.Fatalf("tap pool after cleanup state=%s owner=%q ok=%v, want Ready with no owner", poolState, owner, ok)
	}
	taps, err = controller.ListTaps(ctx, &ListTapsRequest{})
	if err != nil {
		t.Fatalf("ListTaps after cleanup returned error: %v", err)
	}
	if len(taps.Taps) != 1 || taps.Taps[0].State != string(TapPoolReady) || taps.Taps[0].OwnerSandboxID != "" {
		t.Fatalf("ListTaps after cleanup=%#v, want Ready free tap", taps.Taps)
	}
	if taps.StateCounts[string(TapPoolReady)] != 1 {
		t.Fatalf("ListTaps stateCounts after cleanup=%v, want one Ready", taps.StateCounts)
	}
	if taps.StateCounts[string(TapPoolActive)] != 0 {
		t.Fatalf("ListTaps stateCounts after cleanup=%v, want no Active networks", taps.StateCounts)
	}
	cubevsAdapter := controller.cubevsAdapter.(*fakeCubeVSAdapter)
	if len(cubevsAdapter.deletePortMappings) != 1 || cubevsAdapter.deletePortMappings[0].HostPort != hostPort {
		t.Fatalf("deletePortMappings=%#v, want hostPort %d", cubevsAdapter.deletePortMappings, hostPort)
	}
	cubeEgressAdapter := controller.cubeEgressAdapter.(*fakeCubeEgressAdapter)
	if cubeEgressAdapter.deleteCalls != 1 || cubeEgressAdapter.verifyCalls != 1 {
		t.Fatalf("cubeegress deleteCalls=%d verifyCalls=%d, want 1/1", cubeEgressAdapter.deleteCalls, cubeEgressAdapter.verifyCalls)
	}

	reused, err := controller.EnsureNetwork(ctx, &EnsureNetworkRequest{
		SandboxID: "sandbox-integration-b",
		PortMappings: []PortMapping{{
			HostPort:      hostPort,
			ContainerPort: 9090,
		}},
	})
	if err != nil {
		t.Fatalf("EnsureNetwork after cleanup returned error: %v", err)
	}
	if len(reused.PortMappings) != 1 || reused.PortMappings[0].HostPort != hostPort {
		t.Fatalf("reused port mappings = %#v, want hostPort %d", reused.PortMappings, hostPort)
	}
	if reused.Interfaces[0].Name != tapName {
		t.Fatalf("reused tap=%s, want cleaned tap %s", reused.Interfaces[0].Name, tapName)
	}
}

func TestNetworkRuntimeRecoverSuccessThenReleaseCleanIntegration(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	store, err := newStateStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	initial := &persistedState{
		SandboxID:     "sandbox-recover-integration",
		NetworkHandle: "sandbox-recover-integration",
		TapName:       tapName("10.40.0.2"),
		TapIfIndex:    42,
		SandboxIP:     "10.40.0.2",
		Interfaces: []Interface{{
			Name:    tapName("10.40.0.2"),
			MAC:     createTestConfig().MVMMacAddr,
			MTU:     int32(createTestConfig().MvmMtu),
			IPs:     []string{"169.254.68.6/30"},
			Gateway: createTestConfig().MvmGwDestIP,
		}},
		PortMappings: []PortMapping{{
			Protocol:      "tcp",
			HostIP:        "127.0.0.1",
			HostPort:      20042,
			ContainerPort: 8080,
		}},
		PersistMetadata: map[string]string{"sandbox_ip": "10.40.0.2"},
	}
	if err := store.WriteTmp(initial); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitCreating(initial.SandboxID); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitSuccess(initial.SandboxID); err != nil {
		t.Fatal(err)
	}

	// Simulate a process restart with durable success state and a matching live
	// TAP. Recovery should restore ownership, fd handoff, and host-port conflicts;
	// release+cleanup should then free those resources.
	allocator, err := newIPAllocator("10.40.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	ports, err := newPortBinder()
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewTapPool()
	if err != nil {
		t.Fatal(err)
	}
	tapIP := net.ParseIP("10.40.0.2").To4()
	tap := &tapDevice{Name: initial.TapName, Index: 42, IP: tapIP}
	cubevsAdapter := &fakeCubeVSAdapter{
		getTAPDeviceByIndex: map[uint32]*cubevs.TAPDevice{
			42: {ID: initial.SandboxID, Ifindex: 42, IP: tapIP},
		},
	}
	controller, err := newNetworkControllerFromDeps(createTestConfig(), networkControllerDeps{
		store:             store,
		allocator:         allocator,
		ports:             ports,
		tapAdapter:        &fakeTapDeviceAdapter{listResult: map[string]*tapDevice{tapIP.String(): tap}},
		cubevsAdapter:     cubevsAdapter,
		cubeEgressAdapter: &fakeCubeEgressAdapter{configured: true},
		tapPool:           pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	controller.cubeDev = &systemnet.CubeDev{Index: 100}

	if err := controller.recover(); err != nil {
		t.Fatalf("recover returned error: %v", err)
	}
	file, err := controller.GetTapFile(initial.SandboxID, initial.TapName)
	if err != nil {
		t.Fatalf("GetTapFile after recover returned error: %v", err)
	}
	_ = file.Close()
	poolState, owner, ok := controller.tapPool.StateByName(initial.TapName)
	if !ok || poolState != TapPoolActive || owner != initial.SandboxID {
		t.Fatalf("tap pool after recover state=%s owner=%q ok=%v, want Active owner", poolState, owner, ok)
	}
	if _, err := controller.ports.Reserve("other", initial.PortMappings, controller.cfg.HostPortBindIP); err == nil {
		t.Fatal("recovered hostPort reservation succeeded for another owner before release; want conflict")
	}

	if _, err := controller.ReleaseNetwork(ctx, &ReleaseNetworkRequest{SandboxID: initial.SandboxID}); err != nil {
		t.Fatalf("ReleaseNetwork after recover returned error: %v", err)
	}
	if controller.store.Exists(initial.SandboxID, StateFileDeleting) || controller.store.Exists(initial.SandboxID, StateFileSuccess) {
		t.Fatal("state file still exists after recover release cleanup")
	}
	if _, err := controller.ports.Reserve("other", initial.PortMappings, controller.cfg.HostPortBindIP); err != nil {
		t.Fatalf("hostPort was not released after recover cleanup: %v", err)
	}
}

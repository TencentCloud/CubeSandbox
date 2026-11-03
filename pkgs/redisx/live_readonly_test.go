// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package redisx

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/mna/redisc"
)

type observedCall struct {
	address, command, result string
}

type observedConn struct {
	redis.Conn
	address string
	record  func(observedCall)
}

func (c *observedConn) Do(command string, args ...interface{}) (interface{}, error) {
	v, err := c.Conn.Do(command, args...)
	c.observe(command, args, v, err)
	return v, err
}

func (c *observedConn) DoWithTimeout(timeout time.Duration, command string, args ...interface{}) (interface{}, error) {
	v, err := redis.DoWithTimeout(c.Conn, timeout, command, args...)
	c.observe(command, args, v, err)
	return v, err
}

func (c *observedConn) observe(command string, args []interface{}, value interface{}, err error) {
	command = strings.ToUpper(command)
	if command == "CLUSTER" && len(args) == 1 && fmt.Sprint(args[0]) == "SLOTS" {
		command = "CLUSTER SLOTS"
	} else if command != "MIGRATE" && command != "SCAN" {
		return
	}
	result := "OK"
	if err != nil {
		result = err.Error()
	} else if command == "MIGRATE" {
		result, _ = redis.String(value, nil)
	}
	c.record(observedCall{c.address, command, result})
}

func observeClient(c *Cluster) func() []observedCall {
	var mu sync.Mutex
	var calls []observedCall
	createPool := c.cluster.CreatePool
	c.cluster.CreatePool = func(addr string, opts ...redis.DialOption) (*redis.Pool, error) {
		pool, err := createPool(addr, opts...)
		if err != nil {
			return nil, err
		}
		dial := pool.Dial
		pool.Dial = func() (redis.Conn, error) {
			conn, err := dial()
			if err != nil {
				return nil, err
			}
			return &observedConn{Conn: conn, address: addr, record: func(call observedCall) {
				mu.Lock()
				calls = append(calls, call)
				mu.Unlock()
			}}, nil
		}
		return pool, nil
	}
	return func() []observedCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]observedCall(nil), calls...)
	}
}

func nodeAddress(t *testing.T, value interface{}) string {
	t.Helper()
	entry, err := redis.Values(value, nil)
	if err != nil || len(entry) < 2 {
		t.Fatalf("invalid node: %v", err)
	}
	host, err := redis.String(entry[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	port, err := redis.Int(entry[1], nil)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

func waitRole(conn redis.Conn, want string) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		role, err := redis.Values(conn.Do("ROLE"))
		if err != nil {
			return err
		}
		name, _ := redis.String(role[0], nil)
		if name == want {
			if want != "slave" {
				return nil
			}
			state, _ := redis.String(role[3], nil)
			if state == "connected" {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("role did not become %s", want)
}

func waitReplicaTopology(primary redis.Conn, address string) error {
	id, err := redis.String(primary.Do("CLUSTER", "MYID"))
	if err != nil {
		return err
	}
	nodes, err := redis.String(primary.Do("CLUSTER", "NODES"))
	if err != nil {
		return err
	}
	var observers []redis.Conn
	defer func() {
		for _, conn := range observers {
			conn.Close()
		}
	}()
	for _, row := range strings.Split(strings.TrimSpace(nodes), "\n") {
		fields := strings.Fields(row)
		if len(fields) < 8 {
			return fmt.Errorf("invalid cluster node row")
		}
		conn, err := liveDial(strings.Split(fields[1], "@")[0])
		if err != nil {
			return err
		}
		observers = append(observers, conn)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		converged := true
		for _, observer := range observers {
			rows, err := redis.Strings(observer.Do("CLUSTER", "REPLICAS", id))
			found := false
			if err == nil {
				for _, row := range rows {
					fields := strings.Fields(row)
					if len(fields) > 7 && strings.Split(fields[1], "@")[0] == address && fields[7] == "connected" {
						found = true
					}
				}
			}
			converged = converged && found
		}
		if converged {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("cluster topology did not register replica %s", address)
}

// A keyed write is redirected with MOVED before Redis checks replica write
// protection. Empty-key MIGRATE reaches the native READONLY guard without
// touching data or opening a migration connection. No response is synthesized.
func TestLiveReadOnlyRefreshAfterPromotion(t *testing.T) {
	if os.Getenv("TEST_REDIS_CLUSTER_FAILOVER") != "1" {
		t.Skip("isolated failover fixture is not enabled")
	}
	c := liveClient(t)
	calls := observeClient(c)
	seed, err := liveDial(strings.Split(os.Getenv("TEST_REDIS_CLUSTER_NODES"), ",")[0])
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	slots, err := redis.Values(seed.Do("CLUSTER", "SLOTS"))
	if err != nil {
		t.Fatal(err)
	}
	// redisc binds non-script commands using the first argument.
	slot := redisc.Slot("127.0.0.1")
	var primaryAddr, replicaAddr string
	for _, row := range slots {
		entry, err := redis.Values(row, nil)
		if err != nil || len(entry) < 3 {
			t.Fatal("invalid slot row")
		}
		lo, _ := redis.Int(entry[0], nil)
		hi, _ := redis.Int(entry[1], nil)
		if slot >= lo && slot <= hi {
			if len(entry) < 4 {
				t.Fatal("target primary has no replica")
			}
			primaryAddr, replicaAddr = nodeAddress(t, entry[2]), nodeAddress(t, entry[3])
		}
	}
	if primaryAddr == "" {
		t.Fatal("target slot missing")
	}
	primary, err := liveDial(primaryAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	replica, err := liveDial(replicaAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	config, err := redis.Strings(primary.Do("CONFIG", "GET", "cluster-node-timeout"))
	if err != nil || len(config) != 2 {
		t.Fatalf("read fixture node timeout: %v", err)
	}
	nodeTimeout, err := time.ParseDuration(config[1] + "ms")
	if err != nil || nodeTimeout <= 0 || nodeTimeout > 10*time.Second {
		t.Fatal("fixture node timeout outside bounded test budget")
	}
	if err = waitReplicaTopology(primary, replicaAddr); err != nil {
		t.Fatal(err)
	}
	if err = c.cluster.Refresh(); err != nil {
		t.Fatal(err)
	}
	args := []interface{}{"127.0.0.1", 1, "", 0, 100, "KEYS"}
	if value, err := redis.String(c.Do("MIGRATE", args...)); err != nil || value != "NOKEY" {
		t.Fatalf("warm primary: %q %v", value, err)
	}
	before := len(calls())
	if _, err = replica.Do("CLUSTER", "FAILOVER"); err != nil {
		t.Fatal(err)
	}
	promotedAt := time.Now()
	defer func() {
		// Redis retains election state for four node timeouts. Reversing a
		// promotion immediately can reuse the previous election's epoch.
		if remaining := time.Until(promotedAt.Add(4*nodeTimeout + time.Second)); remaining > 0 {
			time.Sleep(remaining)
		}
		if err := waitRole(primary, "slave"); err != nil {
			t.Error(err)
			return
		}
		if err := waitReplicaTopology(replica, primaryAddr); err != nil {
			t.Error(err)
			return
		}
		if _, err := primary.Do("CLUSTER", "FAILOVER"); err != nil {
			t.Error(err)
			return
		}
		if err := waitRole(primary, "master"); err != nil {
			t.Error(err)
			return
		}
		if err := waitRole(replica, "slave"); err != nil {
			t.Error(err)
			return
		}
		if err := waitReplicaTopology(primary, replicaAddr); err != nil {
			t.Error(err)
			return
		}
		t.Logf("restored_primary=%s restored_replica=%s", primaryAddr, replicaAddr)
	}()
	if err = waitRole(primary, "slave"); err != nil {
		t.Fatal(err)
	}
	if err = waitRole(replica, "master"); err != nil {
		t.Fatal(err)
	}
	value, err := redis.String(c.Do("MIGRATE", args...))
	if err != nil || value != "NOKEY" {
		t.Fatalf("retry result=%q err=%v", value, err)
	}
	trace := calls()[before:]
	var attempts []observedCall
	refreshes := 0
	for _, call := range trace {
		t.Logf("address=%s command=%s result=%q", call.address, call.command, call.result)
		if call.command == "MIGRATE" {
			attempts = append(attempts, call)
		}
		if call.command == "CLUSTER SLOTS" && call.result == "OK" {
			refreshes++
		}
	}
	if len(attempts) != 2 || attempts[0].address != primaryAddr ||
		!strings.HasPrefix(attempts[0].result, "READONLY ") ||
		attempts[1].address != replicaAddr || attempts[1].result != "NOKEY" || refreshes != 1 {
		t.Fatalf("unexpected retry trace: %+v refreshes=%d", attempts, refreshes)
	}
	t.Logf("native_readonly=1 successful_retries=1 topology_refreshes=%d data_keys_touched=0", refreshes)
}

func TestLiveScanUsesPrimaries(t *testing.T) {
	c := liveClient(t)
	calls := observeClient(c)
	seed, err := liveDial(strings.Split(os.Getenv("TEST_REDIS_CLUSTER_NODES"), ",")[0])
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	slots, err := redis.Values(seed.Do("CLUSTER", "SLOTS"))
	if err != nil {
		t.Fatal(err)
	}
	primaries := map[string]bool{}
	for _, row := range slots {
		entry, err := redis.Values(row, nil)
		if err != nil || len(entry) < 3 {
			t.Fatal("invalid slot row")
		}
		primaries[nodeAddress(t, entry[2])] = true
	}
	if len(primaries) < 2 {
		t.Fatal("fixture must have more than one primary")
	}
	if _, err = c.ScanKeys("cube-validation-scan-target:*"); err != nil {
		t.Fatal(err)
	}
	visited := map[string]bool{}
	for _, call := range calls() {
		if call.command == "SCAN" {
			t.Logf("scan_address=%s current_primary=%t", call.address, primaries[call.address])
			if !primaries[call.address] {
				t.Errorf("SCAN reached non-primary %s", call.address)
			}
			visited[call.address] = true
		}
	}
	if len(visited) != len(primaries) {
		t.Fatalf("visited %d of %d primaries", len(visited), len(primaries))
	}
}

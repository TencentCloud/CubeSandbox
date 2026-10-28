// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
package webhook

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gomodule/redigo/redis"
	"github.com/stretchr/testify/require"
)

// Opt-in deployment fixture: the actual Master connects to this RESP proxy.
// Reuses the component fault server; this test process is only a fault injector,
// never evidence that a complete Master or a node ran. No synthetic event is put.
// control.json: {"id":1,"op":"claim","when":"after"}; each id fires once
// unless a finite repeat count is requested. Error replies apply before execution.
// Claims only fire after Redis returned an actual task/Receipt, not an idle poll.
func TestP6PlatformRedisProxy(t *testing.T) {
	dir := os.Getenv("WEBHOOK_P6_PROXY_DIR")
	if dir == "" {
		t.Skip("deployment fault proxy not requested")
	}
	upstream := os.Getenv("WEBHOOK_P6_PROXY_UPSTREAM")
	// Match the explicitly inventoried Docker publication, not a developer's port.
	container := os.Getenv("WEBHOOK_P6_PROXY_REDIS_CONTAINER")
	label := os.Getenv("WEBHOOK_P6_RESOURCE_LABEL")
	require.True(t, strings.HasPrefix(container, "webhook-1903-"))
	require.True(t, strings.HasPrefix(label, "webhook-p6-") && len(label) > len("webhook-p6-"))
	host, port, err := net.SplitHostPort(upstream)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", host, "proxy upstream must be a dedicated loopback publication")
	raw, err := exec.Command("docker", "inspect", container).Output()
	require.NoError(t, err)
	var inventory []struct {
		Config          struct{ Labels map[string]string }
		NetworkSettings struct {
			Ports map[string][]struct{ HostIp, HostPort string }
		}
	}
	require.NoError(t, json.Unmarshal(raw, &inventory))
	require.Len(t, inventory, 1)
	require.Equal(t, label, inventory[0].Config.Labels["purpose"])
	matched := false
	for _, binding := range inventory[0].NetworkSettings.Ports["6379/tcp"] {
		matched = matched || (binding.HostIp == host && binding.HostPort == port)
	}
	require.True(t, matched, "upstream must match the inventoried Redis container publication")
	up, err := redis.Dial("tcp", upstream, redis.DialReadTimeout(2*time.Second), redis.DialWriteTimeout(2*time.Second))
	require.NoError(t, err)
	t.Cleanup(func() { _ = up.Close() })
	var mu sync.Mutex
	used := map[int]int{}
	addr := respServer(t, func(cmd string, args []interface{}) (interface{}, bool) {
		// A single upstream serializes this precise fault experiment. Performance
		// and normal multi-Worker measurements must use direct Redis connections.
		mu.Lock()
		defer mu.Unlock()
		op := cmd
		if (cmd == "EVAL" || cmd == "EVALSHA") && len(args) >= 2 {
			n, e := redis.Int(args[1], nil)
			if e == nil && n >= 0 && len(args) > n+2 {
				op, _ = redis.String(args[n+2], nil)
			}
		}
		var fault struct {
			ID        int    `json:"id"`
			Op        string `json:"op"`
			When      string `json:"when"`
			Key       string `json:"key"`
			Field     string `json:"field"`
			Error     bool   `json:"error"`
			Repeat    int    `json:"repeat"`
			KeyPrefix string `json:"key_prefix"`
		}
		body, _ := os.ReadFile(filepath.Join(dir, "control.json"))
		_ = json.Unmarshal(body, &fault)
		if fault.Repeat <= 0 {
			fault.Repeat = 1
		}
		armed := fault.ID > 0 && fault.Repeat <= 64 && used[fault.ID] < fault.Repeat && fault.Op == op
		if armed && fault.Key != "" {
			if len(args) == 0 {
				armed = false
			} else {
				value, _ := redis.String(args[0], nil)
				armed = value == fault.Key
			}
		}
		if armed && fault.KeyPrefix != "" {
			value := ""
			if len(args) > 0 {
				value, _ = redis.String(args[0], nil)
			}
			armed = strings.HasPrefix(value, fault.KeyPrefix)
		}
		if armed && fault.Field != "" {
			if len(args) < 2 {
				armed = false
			} else {
				value, _ := redis.String(args[1], nil)
				armed = value == fault.Field
			}
		}
		if armed && op == "claim" && fault.When == "before" {
			// Target a due persisted task, rather than consuming the fault on an
			// idle poll. This read belongs only to the external test injector.
			if len(args) < 7 {
				armed = false
			} else {
				n, e := redis.Int(up.Do("ZCOUNT", args[3], "-inf", args[6]))
				armed = e == nil && n > 0
			}
		}
		fired := func(executed bool, claimValues int) {
			used[fault.ID]++
			evidence := map[string]any{"id": fault.ID, "op": op,
				"upstream_executed": executed, "client_reply": fault.Error, "server_error": fault.Error,
				"claim_values": claimValues, "at": time.Now().UTC()}
			evidence["hit"] = used[fault.ID]
			if len(args) > 0 && (fault.KeyPrefix != "" || fault.Key != "") {
				evidence["matched_key"], _ = redis.String(args[0], nil)
			}
			if op == "put" && len(args) >= 8 {
				// Dedicated test bodies/targets only, retained as raw input evidence.
				evidence["immutable_json"], _ = redis.String(args[7], nil)
			}
			v, _ := json.Marshal(evidence)
			name := filepath.Join(dir, "fault-"+strconv.Itoa(fault.ID)+".json")
			if used[fault.ID] > 1 {
				name = filepath.Join(dir, "fault-"+strconv.Itoa(fault.ID)+"-"+strconv.Itoa(used[fault.ID])+".json")
			}
			if e := os.WriteFile(name+".tmp", v, 0600); e != nil {
				t.Errorf("write fault evidence: %v", e)
			} else if e := os.Rename(name+".tmp", name); e != nil {
				t.Errorf("publish fault evidence: %v", e)
			}
			t.Log(string(v))
		}
		if armed && fault.When == "before" {
			fired(false, 0)
			if fault.Error {
				return redis.Error("ERR P6 dedicated injected failure"), false
			}
			return nil, true
		}
		v, e := up.Do(cmd, args...)
		if e != nil {
			return e, false
		}
		claim, _ := redis.Values(v, nil)
		if armed && fault.When == "after" && (op != "claim" || len(claim) > 2) {
			fired(true, len(claim))
			return nil, true
		}
		return v, false
	})
	f, err := os.OpenFile(filepath.Join(dir, "ready"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	require.NoError(t, err)
	_, err = f.WriteString(addr)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	deadline := time.NewTimer(30 * time.Minute)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("deployment fixture exceeded its bounded lifetime")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

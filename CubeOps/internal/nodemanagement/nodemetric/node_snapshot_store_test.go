// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package nodemetric

import (
	"testing"

	"github.com/gomodule/redigo/redis"
	"github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/model"
)

type recordedCmd struct {
	name string
	args []any
}

type fakeSnapshotConn struct {
	cmds *[]recordedCmd
}

func (c *fakeSnapshotConn) Close() error { return nil }
func (c *fakeSnapshotConn) Err() error   { return nil }
func (c *fakeSnapshotConn) Do(commandName string, args ...any) (any, error) {
	*c.cmds = append(*c.cmds, recordedCmd{name: commandName, args: append([]any(nil), args...)})
	return "OK", nil
}
func (c *fakeSnapshotConn) Send(string, ...any) error { return nil }
func (c *fakeSnapshotConn) Flush() error              { return nil }
func (c *fakeSnapshotConn) Receive() (any, error)     { return nil, nil }

func TestWriteAndDeleteNodeSnapshotSyncsEndpointHash(t *testing.T) {
	var cmds []recordedCmd
	savedPool := pool
	pool = &redis.Pool{
		Dial: func() (redis.Conn, error) {
			return &fakeSnapshotConn{cmds: &cmds}, nil
		},
	}
	t.Cleanup(func() { pool = savedPool })
	t.Cleanup(SetNodeSnapshotHooks(nil, nil))

	WriteNodeSnapshot(&model.NodeSnapshot{NodeID: "node-a", HostIP: "10.0.0.11"})
	WriteNodeSnapshot(&model.NodeSnapshot{NodeID: "node-a", HostIP: "10.0.0.22"})
	DeleteNodeSnapshot("node-a")

	var hsets []recordedCmd
	var hdels []recordedCmd
	for _, cmd := range cmds {
		switch cmd.name {
		case "HSET":
			hsets = append(hsets, cmd)
		case "HDEL":
			hdels = append(hdels, cmd)
		}
	}
	if len(hsets) != 2 {
		t.Fatalf("expected 2 HSET endpoint commands, got %d (%+v)", len(hsets), cmds)
	}
	if hsets[0].args[0] != NodeEndpointHashKeyForTest() || hsets[0].args[1] != "node-a" || hsets[0].args[2] != "10.0.0.11" {
		t.Fatalf("first HSET = %+v", hsets[0].args)
	}
	if hsets[1].args[0] != NodeEndpointHashKeyForTest() || hsets[1].args[1] != "node-a" || hsets[1].args[2] != "10.0.0.22" {
		t.Fatalf("second HSET = %+v", hsets[1].args)
	}
	if len(hdels) != 1 || hdels[0].args[0] != NodeEndpointHashKeyForTest() || hdels[0].args[1] != "node-a" {
		t.Fatalf("expected HDEL %s node-a, got %+v", NodeEndpointHashKeyForTest(), hdels)
	}
}

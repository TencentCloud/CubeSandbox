// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//go:build cgo && linux

package cubecow

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"testing"
)

// Exercise the real Go -> C -> Rust binding against the existing RPC contract.
func TestMaterialiseExportNativeBinding(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "rpc.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		decoder, encoder := json.NewDecoder(conn), json.NewEncoder(conn)
		for i := 0; i < 3; i++ {
			var req struct {
				ID     uint64            `json:"id"`
				Method string            `json:"method"`
				Params map[string]string `json:"params"`
			}
			if err = decoder.Decode(&req); err != nil {
				done <- err
				return
			}
			want := "rcow_materialise_export"
			value := "original-uuid"
			if i == 0 {
				want = "rcow_deactive_bdev"
				value = "already_deactivated"
			} else if req.Params["export_uuid"] != "original-uuid" {
				done <- fmt.Errorf("wrong UUID: %v", req.Params)
				return
			}
			if req.Method != want {
				done <- fmt.Errorf("wrong method: %s", req.Method)
				return
			}
			if i == 2 {
				value = "Device or resource busy"
			}
			if err = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"bool_value": i != 2, "string_value": value}}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	config, _ := json.Marshal(map[string]any{"log": map[string]any{}, "backend": map[string]any{"kind": "s3", "s3": map[string]any{"socket_path": socket, "state_dir": filepath.Join(dir, "state")}}})
	engine, err := InitWithoutLoggingFromJSON(string(config))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err = engine.MaterialiseExport("original-uuid"); err != nil {
		t.Fatal(err)
	}
	if err = engine.MaterialiseExport("original-uuid"); err == nil {
		t.Fatal("native binding swallowed backend failure")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

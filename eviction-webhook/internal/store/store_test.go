// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tencentcloud/CubeSandbox/eviction-webhook/pkg/types"
)

func TestStoreSaveAndRecover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")

	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	event := &types.EvictionEvent{
		EventID:       "evt-1",
		PodName:       "sandbox-a",
		Namespace:     "cube-system",
		NodeName:      "node-1",
		InstanceType:  "cubebox",
		InterceptedAt: "2026-07-23T10:00:00Z",
	}

	if err := s.Save(event); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s.Close()

	// Verify file content: one JSON line
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var recovered types.EvictionEvent
	if err := json.Unmarshal(data[:len(data)-1], &recovered); err != nil {
		t.Fatalf("Unmarshal: %v (data=%q)", err, string(data))
	}

	if recovered.EventID != event.EventID {
		t.Errorf("EventID: want %s, got %s", event.EventID, recovered.EventID)
	}
	if recovered.PodName != event.PodName {
		t.Errorf("PodName: want %s, got %s", event.PodName, recovered.PodName)
	}
	if recovered.Namespace != event.Namespace {
		t.Errorf("Namespace: want %s, got %s", event.Namespace, recovered.Namespace)
	}
	if recovered.NodeName != event.NodeName {
		t.Errorf("NodeName: want %s, got %s", event.NodeName, recovered.NodeName)
	}
	if recovered.InstanceType != event.InstanceType {
		t.Errorf("InstanceType: want %s, got %s", event.InstanceType, recovered.InstanceType)
	}
}

func TestStoreMultipleRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")

	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 0; i < 5; i++ {
		event := &types.EvictionEvent{
			EventID:       fmt.Sprintf("evt-%d", i),
			PodName:       fmt.Sprintf("sandbox-%d", i),
			Namespace:     "cube-system",
			NodeName:      "node-1",
			InstanceType:  "cubebox",
			InterceptedAt: "2026-07-23T10:00:00Z",
		}
		if err := s.Save(event); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}
	s.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Count lines — each line is one NDJSON record
	lines := splitLines(string(data))
	if len(lines) != 5 {
		t.Errorf("expected 5 lines, got %d: %q", len(lines), string(data))
	}
}

func TestStoreConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")

	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			event := &types.EvictionEvent{
				EventID:       fmt.Sprintf("evt-%d", idx),
				PodName:       fmt.Sprintf("sandbox-%d", idx),
				Namespace:     "cube-system",
				NodeName:      "node-1",
				InstanceType:  "cubebox",
				InterceptedAt: "2026-07-23T10:00:00Z",
			}
			if err := s.Save(event); err != nil {
				t.Errorf("Save %d: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()
	s.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := splitLines(string(data))
	if len(lines) != goroutines {
		t.Errorf("expected %d lines, got %d", goroutines, len(lines))
	}
}

func TestStoreCreatesFileIfNotExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent", "events.ndjson")

	s, err := New(path)
	if err != nil {
		t.Fatalf("New should create parent dirs: %v", err)
	}
	_ = s.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("expected empty file, got %d bytes", len(data))
	}
}

func TestStoreClosedFileReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")

	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.Close()

	err = s.Save(&types.EvictionEvent{EventID: "after-close", PodName: "p", Namespace: "ns", InterceptedAt: "t"})
	if err == nil {
		t.Error("expected error on Save after Close")
	}
}

func splitLines(s string) []string {
	var lines []string
	var current []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, string(current))
			current = nil
		} else {
			current = append(current, s[i])
		}
	}
	if len(current) > 0 {
		lines = append(lines, string(current))
	}
	return lines
}

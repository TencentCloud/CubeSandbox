// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

const testSandboxID = "c684b8e745484767bdad83c854ba2a2f"

// writeShimLog writes content to a temporary shim log and returns its path.
func writeShimLog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cube-shim-req.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func shimLine(sandboxID, ts, msg string) string {
	return `{"Module":"cube-shim","InstanceId":"` + sandboxID + `","ContainerId":"c1","Timestamp":"` + ts +
		`","LogContent":"` + msg + `","FunctionType":"req"}` + "\n"
}

// marshalRes serialises the response the way handleSandboxLogsAction does and
// reports whether the body carries an explicit null logs field.
func marshalRes(t *testing.T, res *SandboxLogsRes) string {
	t.Helper()
	body, err := fastJSON.Marshal(res)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return string(body)
}

// An empty file is the shape that used to produce "logs":null and make CubeAPI
// answer 500 for both GET /sandboxes/{id}/logs and GET /v2/sandboxes/{id}/logs.
func TestReadShimLogsEmptyFileEncodesEmptyArray(t *testing.T) {
	entries, nextCursor, hasMore, err := readShimLogs(writeShimLog(t, ""), testSandboxID, 0, defaultLogLimit)
	if err != nil {
		t.Fatalf("readShimLogs: %v", err)
	}
	if entries == nil {
		t.Fatal("entries must not be nil")
	}
	if len(entries) != 0 || nextCursor != 0 || hasMore {
		t.Fatalf("unexpected result: len=%d nextCursor=%d hasMore=%v", len(entries), nextCursor, hasMore)
	}

	body := marshalRes(t, newSandboxLogsRes(&types.Ret{RetCode: 0}, entries))
	if !strings.Contains(body, `"logs":[]`) {
		t.Fatalf("logs must encode as [] rather than null: %s", body)
	}
}

// Lines for other sandboxes must not leak into the result, and the response
// must still encode an empty array.
func TestReadShimLogsNoMatchEncodesEmptyArray(t *testing.T) {
	content := shimLine("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "2026-11-24T06:03:55.455501721Z", "other sandbox")
	entries, _, _, err := readShimLogs(writeShimLog(t, content), testSandboxID, 0, defaultLogLimit)
	if err != nil {
		t.Fatalf("readShimLogs: %v", err)
	}
	if entries == nil {
		t.Fatal("entries must not be nil")
	}
	if len(entries) != 0 {
		t.Fatalf("expected no entries, got %d", len(entries))
	}

	body := marshalRes(t, newSandboxLogsRes(&types.Ret{RetCode: 0}, entries))
	if !strings.Contains(body, `"logs":[]`) {
		t.Fatalf("logs must encode as [] rather than null: %s", body)
	}
}

// Matching lines are still returned unchanged.
func TestReadShimLogsMatchingEntries(t *testing.T) {
	content := shimLine("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "2026-11-24T06:03:55.455501721Z", "other sandbox") +
		shimLine(testSandboxID, "2026-11-24T06:03:56.000000000Z", "create req start") +
		shimLine(testSandboxID, "2026-11-24T06:03:57.000000000Z", "load spec finish at:0")

	entries, nextCursor, hasMore, err := readShimLogs(writeShimLog(t, content), testSandboxID, 0, defaultLogLimit)
	if err != nil {
		t.Fatalf("readShimLogs: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Message != "create req start" || entries[1].Message != "load spec finish at:0" {
		t.Fatalf("unexpected messages: %+v", entries)
	}
	if entries[0].Level != "info" || entries[0].Timestamp != "2026-11-24T06:03:56.000000000Z" {
		t.Fatalf("unexpected first entry: %+v", entries[0])
	}
	if nextCursor == 0 || hasMore {
		t.Fatalf("unexpected pagination state: nextCursor=%d hasMore=%v", nextCursor, hasMore)
	}

	body := marshalRes(t, newSandboxLogsRes(&types.Ret{RetCode: 0}, entries))
	if !strings.Contains(body, `"message":"create req start"`) {
		t.Fatalf("entries must be serialised: %s", body)
	}
}

// A nil slice handed to the constructor must still encode as [].
func TestNewSandboxLogsResNeverEncodesNull(t *testing.T) {
	body := marshalRes(t, newSandboxLogsRes(&types.Ret{RetCode: 0}, nil))
	if strings.Contains(body, `"logs":null`) || !strings.Contains(body, `"logs":[]`) {
		t.Fatalf("nil logs must be normalised to []: %s", body)
	}
}

// A missing file is an error, but the invariant "never nil" still holds.
func TestReadShimLogsMissingFileReturnsEmptySlice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.log")
	entries, _, _, err := readShimLogs(path, testSandboxID, 0, defaultLogLimit)
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if entries == nil {
		t.Fatal("entries must not be nil even on error")
	}
	if len(entries) != 0 {
		t.Fatalf("expected no entries, got %d", len(entries))
	}
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

// writeShimLogLines appends JSON LogItem lines to path.
func writeShimLogLines(t *testing.T, path string, lines []string) {
	t.Helper()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

// shimLine builds a CubeShim LogItem JSON line.
func shimLine(instanceID, ts, content string) string {
	return `{"Module":"Shim","InstanceId":"` + instanceID + `","ContainerId":"` + instanceID + `","Timestamp":"` + ts + `","LogContent":"` + content + `","FunctionType":"cubebox"}`
}

func TestReadShimEventsNewest_ReturnsNewestLimitOldestFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cube-shim-req.log")
	writeShimLogLines(t, path, []string{
		shimLine("sb-aaa", "2026-07-22T10:00:00Z", "oldest"),
		shimLine("sb-aaa", "2026-07-22T10:00:01Z", "middle"),
		shimLine("sb-aaa", "2026-07-22T10:00:02Z", "newest"),
	})

	events, err := readShimEventsNewest(context.Background(), path, "sb-aaa", 2)
	if err != nil {
		t.Fatalf("readShimEventsNewest: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 newest events, got %d", len(events))
	}
	if events[0].GetMessage() != "middle" || events[1].GetMessage() != "newest" {
		t.Fatalf("want newest two oldest-first, got %+v", events)
	}
}

func TestReadShimEventsNewest_AllWhenUnderLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cube-shim-req.log")
	writeShimLogLines(t, path, []string{
		shimLine("sb-aaa", "2026-07-22T10:00:00Z", "one"),
		shimLine("sb-aaa", "2026-07-22T10:00:01Z", "two"),
	})

	events, err := readShimEventsNewest(context.Background(), path, "sb-aaa", 200)
	if err != nil {
		t.Fatalf("readShimEventsNewest: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want all 2 events, got %d", len(events))
	}
	if events[0].GetMessage() != "one" || events[1].GetMessage() != "two" {
		t.Fatalf("want oldest-first order, got %+v", events)
	}
}

func TestReadShimEventsNewest_FiltersOtherSandboxes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cube-shim-req.log")
	writeShimLogLines(t, path, []string{
		shimLine("sb-other", "2026-07-22T10:00:00Z", "other-1"),
		shimLine("sb-target", "2026-07-22T10:00:01Z", "target-1"),
		shimLine("sb-other", "2026-07-22T10:00:02Z", "other-2"),
		shimLine("sb-target", "2026-07-22T10:00:03Z", "target-2"),
	})

	events, err := readShimEventsNewest(context.Background(), path, "sb-target", 200)
	if err != nil {
		t.Fatalf("readShimEventsNewest: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want only the target sandbox events, got %d", len(events))
	}
	if events[0].GetMessage() != "target-1" || events[1].GetMessage() != "target-2" {
		t.Fatalf("want target events in order, got %+v", events)
	}
}

func TestReadShimEventsNewest_RespectsScanWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cube-shim-req.log")
	// A large burst of another sandbox's events pushes the target's events
	// beyond maxScanBytes; only events inside the scan window are returned.
	writeShimLogLines(t, path, []string{
		shimLine("sb-old", "2026-07-22T10:00:00Z", "before-window"),
	})
	pad := strings.Repeat("x", int(maxScanBytes))
	writeShimLogLines(t, path, []string{
		shimLine("sb-other", "2026-07-22T10:00:01Z", pad),
		shimLine("sb-target", "2026-07-22T10:00:02Z", "in-window"),
	})

	events, err := readShimEventsNewest(context.Background(), path, "sb-target", 200)
	if err != nil {
		t.Fatalf("readShimEventsNewest: %v", err)
	}
	if len(events) != 1 || events[0].GetMessage() != "in-window" {
		t.Fatalf("want only the in-window event, got %+v", events)
	}
}

func TestReadShimEventsNewest_ScansPastOtherSandboxBurstToFillLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cube-shim-req.log")
	// A burst of another sandbox's events before the target's, proving the scan
	// continues past non-matching lines to fill the limit.
	writeShimLogLines(t, path, []string{
		shimLine("sb-target", "2026-07-22T10:00:00Z", "target-oldest"),
		shimLine("sb-other", "2026-07-22T10:00:01Z", "other-1"),
		shimLine("sb-other", "2026-07-22T10:00:02Z", "other-2"),
		shimLine("sb-target", "2026-07-22T10:00:03Z", "target-newest"),
	})

	events, err := readShimEventsNewest(context.Background(), path, "sb-target", 200)
	if err != nil {
		t.Fatalf("readShimEventsNewest: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 target events, got %d", len(events))
	}
	if events[0].GetMessage() != "target-oldest" || events[1].GetMessage() != "target-newest" {
		t.Fatalf("want both target events, got %+v", events)
	}
}

func TestReadShimEventsNewest_CrossChunkBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cube-shim-req.log")
	big := strings.Repeat("x", scannerBufSize+1)
	writeShimLogLines(t, path, []string{
		shimLine("sb-other", "2026-07-22T10:00:00Z", big),
		shimLine("sb-target", "2026-07-22T10:00:01Z", "target"),
	})

	events, err := readShimEventsNewest(context.Background(), path, "sb-target", 200)
	if err != nil {
		t.Fatalf("readShimEventsNewest: %v", err)
	}
	if len(events) != 1 || events[0].GetMessage() != "target" {
		t.Fatalf("want the target line across a chunk boundary, got %+v", events)
	}
}

func TestReadShimEventsNewest_SkipsInvalidTimestamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cube-shim-req.log")
	writeShimLogLines(t, path, []string{
		shimLine("sb-1", "bad-time", "bad"),
		shimLine("sb-1", "2026-07-22T10:00:01Z", "good"),
	})

	events, err := readShimEventsNewest(context.Background(), path, "sb-1", 200)
	if err != nil {
		t.Fatalf("readShimEventsNewest: %v", err)
	}
	if len(events) != 1 || events[0].GetMessage() != "good" {
		t.Fatalf("want only the valid line, got %+v", events)
	}
}

func TestReadShimEventsNewest_SkipsUnparseableLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cube-shim-req.log")
	writeShimLogLines(t, path, []string{
		"this is not json",
		shimLine("sb-skip", "2026-07-22T10:00:00Z", "valid"),
		"{\"broken\":",
	})

	events, err := readShimEventsNewest(context.Background(), path, "sb-skip", 200)
	if err != nil {
		t.Fatalf("readShimEventsNewest: %v", err)
	}
	if len(events) != 1 || events[0].GetMessage() != "valid" {
		t.Fatalf("want 1 valid event, got %+v", events)
	}
}

func TestReadShimEventsNewest_MissingFileReturnsError(t *testing.T) {
	_, err := readShimEventsNewest(context.Background(), filepath.Join(t.TempDir(), "nope.log"), "sb-1", 10)
	if err == nil {
		t.Fatal("want error for missing file")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("want os.IsNotExist, got %v", err)
	}
}

func TestReadShimEventsNewest_EmptyFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cube-shim-req.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	f.Close()

	events, err := readShimEventsNewest(context.Background(), path, "sb-1", 200)
	if err != nil {
		t.Fatalf("readShimEventsNewest: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("want 0 events for empty file, got %d", len(events))
	}
}

func TestGetSandboxEvents_MissingFileReturnsEmptyPage(t *testing.T) {
	s := &service{}
	t.Setenv("CUBELET_SHIM_REQ_LOG", filepath.Join(t.TempDir(), "missing.log"))
	rsp, err := s.GetSandboxEvents(context.Background(), &cubebox.GetSandboxEventsRequest{
		SandboxID: "sb-missing",
	})
	if err != nil {
		t.Fatalf("GetSandboxEvents: %v", err)
	}
	if rsp.GetRet().GetRetCode() != errorcode.ErrorCode_Success {
		t.Fatalf("want success ret for missing file, got %d", rsp.GetRet().GetRetCode())
	}
	if len(rsp.GetEvents()) != 0 {
		t.Fatalf("want 0 events for missing file, got %d", len(rsp.GetEvents()))
	}
}

func TestGetSandboxEvents_RejectsEmptySandboxID(t *testing.T) {
	s := &service{}
	rsp, err := s.GetSandboxEvents(context.Background(), &cubebox.GetSandboxEventsRequest{})
	if err != nil {
		t.Fatalf("GetSandboxEvents: %v", err)
	}
	if rsp.GetRet().GetRetCode() != errorcode.ErrorCode_InvalidParamFormat {
		t.Fatalf("want InvalidParamFormat, got %d", rsp.GetRet().GetRetCode())
	}
}

func TestGetSandboxEvents_RejectsUnsafeSandboxID(t *testing.T) {
	s := &service{}
	// "../escape" must be rejected by pathutil.ValidateSafeID.
	rsp, err := s.GetSandboxEvents(context.Background(), &cubebox.GetSandboxEventsRequest{
		SandboxID: "../escape",
	})
	if err != nil {
		t.Fatalf("GetSandboxEvents: %v", err)
	}
	if rsp.GetRet().GetRetCode() != errorcode.ErrorCode_InvalidParamFormat {
		t.Fatalf("want InvalidParamFormat for traversal id, got %d", rsp.GetRet().GetRetCode())
	}
}

func TestGetSandboxEvents_DefaultsAndCapsLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cube-shim-req.log")
	lines := make([]string, 10)
	for i := range 10 {
		lines[i] = shimLine("sb-def",
			time.Date(2026, 7, 22, 10, 0, i, 0, time.UTC).Format(time.RFC3339Nano),
			"line")
	}
	writeShimLogLines(t, path, lines)

	t.Setenv("CUBELET_SHIM_REQ_LOG", path)
	s := &service{}

	// limit=0 defaults to 200 → all 10 returned.
	rsp, err := s.GetSandboxEvents(context.Background(), &cubebox.GetSandboxEventsRequest{
		SandboxID: "sb-def",
	})
	if err != nil {
		t.Fatalf("GetSandboxEvents: %v", err)
	}
	if len(rsp.GetEvents()) != 10 {
		t.Fatalf("want 10 events with default limit, got %d", len(rsp.GetEvents()))
	}

	// limit=99999 capped to maxEventLimit → all 10 returned (no panic).
	rsp2, err := s.GetSandboxEvents(context.Background(), &cubebox.GetSandboxEventsRequest{
		SandboxID: "sb-def",
		Limit:     99999,
	})
	if err != nil {
		t.Fatalf("GetSandboxEvents: %v", err)
	}
	if len(rsp2.GetEvents()) != 10 {
		t.Fatalf("want 10 events with capped limit, got %d", len(rsp2.GetEvents()))
	}
}

func TestShimReqLogPath_EnvOverride(t *testing.T) {
	t.Setenv("CUBELET_SHIM_REQ_LOG", "/custom/path.log")
	if got := shimReqLogPath(); got != "/custom/path.log" {
		t.Fatalf("want /custom/path.log, got %s", got)
	}
}

func TestShimReqLogPath_Default(t *testing.T) {
	t.Setenv("CUBELET_SHIM_REQ_LOG", "")
	if got := shimReqLogPath(); got != defaultShimReqLogPath {
		t.Fatalf("want %s, got %s", defaultShimReqLogPath, got)
	}
}

func TestParseShimEvent_SkipsInvalidTimestamp(t *testing.T) {
	cases := []string{"", "not-a-time", "2026-07-22"}

	for _, ts := range cases {
		if _, ok := parseShimEvent([]byte(shimLine("sb-1", ts, "x")), "sb-1"); ok {
			t.Fatalf("want invalid timestamp %q skipped", ts)
		}
	}
}

func TestParseShimEvent_SkipsOtherSandbox(t *testing.T) {
	line := []byte(shimLine("sb-other", "2026-07-22T10:00:00Z", "x"))
	if _, ok := parseShimEvent(line, "sb-1"); ok {
		t.Fatal("want other sandbox line skipped")
	}
}

func TestParseShimEvent_NormalizesTimestampUTC(t *testing.T) {
	line := []byte(shimLine("sb-1", "2026-07-22T10:00:00+08:00", "x"))
	ev, ok := parseShimEvent(line, "sb-1")
	if !ok {
		t.Fatal("want valid timestamp accepted")
	}
	if want := "2026-07-22T02:00:00Z"; ev.GetTimestamp() != want {
		t.Fatalf("want normalized %q, got %q", want, ev.GetTimestamp())
	}
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/tencentcloud/CubeSandbox/pkgs/CubeLog"
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

	events, _, err := readShimEventsNewest(context.Background(), path, "sb-aaa", 2)
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

	events, _, err := readShimEventsNewest(context.Background(), path, "sb-aaa", 200)
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

	events, _, err := readShimEventsNewest(context.Background(), path, "sb-target", 200)
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

	events, _, err := readShimEventsNewest(context.Background(), path, "sb-target", 200)
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

	events, _, err := readShimEventsNewest(context.Background(), path, "sb-target", 200)
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

	events, _, err := readShimEventsNewest(context.Background(), path, "sb-target", 200)
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

	events, _, err := readShimEventsNewest(context.Background(), path, "sb-1", 200)
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

	events, _, err := readShimEventsNewest(context.Background(), path, "sb-skip", 200)
	if err != nil {
		t.Fatalf("readShimEventsNewest: %v", err)
	}
	if len(events) != 1 || events[0].GetMessage() != "valid" {
		t.Fatalf("want 1 valid event, got %+v", events)
	}
}

func TestReadShimEventsNewest_MissingFileReturnsError(t *testing.T) {
	_, _, err := readShimEventsNewest(context.Background(), filepath.Join(t.TempDir(), "nope.log"), "sb-1", 10)
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

	events, _, err := readShimEventsNewest(context.Background(), path, "sb-1", 200)
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

// captureLog redirects CubeLog output for the test and returns the buffer.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	CubeLog.SetOutput(buf)
	t.Cleanup(func() { CubeLog.SetOutput(os.Stdout) })
	return buf
}

// resetMissingLogWarn gives the test a fresh limiter and restores the original.
func resetMissingLogWarn(t *testing.T) {
	t.Helper()
	orig := missingLogWarn
	missingLogWarn = &rate.Sometimes{Interval: time.Hour}
	t.Cleanup(func() { missingLogWarn = orig })
}

func TestGetSandboxEvents_MissingFileWarnsOnceWithPath(t *testing.T) {
	resetMissingLogWarn(t)
	buf := captureLog(t)
	path := filepath.Join(t.TempDir(), "missing.log")
	t.Setenv("CUBELET_SHIM_REQ_LOG", path)
	s := &service{}

	for range 5 {
		rsp, err := s.GetSandboxEvents(context.Background(), &cubebox.GetSandboxEventsRequest{SandboxID: "sb-1"})
		if err != nil || rsp.GetRet().GetRetCode() != errorcode.ErrorCode_Success || len(rsp.GetEvents()) != 0 {
			t.Fatalf("a missing file must still be an empty success: rsp=%+v err=%v", rsp, err)
		}
	}

	out := buf.String()
	if n := strings.Count(out, "shim req log not found"); n != 1 {
		t.Fatalf("want exactly 1 rate-limited warning, got %d: %s", n, out)
	}
	if !strings.Contains(out, path) || !strings.Contains(out, `"LogLevel":"WARN"`) {
		t.Fatalf("warning must be WARN level and name the path %q: %s", path, out)
	}
}

func TestGetSandboxEvents_MissingFileWarnsInTailMode(t *testing.T) {
	resetMissingLogWarn(t)
	buf := captureLog(t)
	t.Setenv("CUBELET_SHIM_REQ_LOG", filepath.Join(t.TempDir(), "missing.log"))

	rsp, err := (&service{}).GetSandboxEvents(context.Background(), &cubebox.GetSandboxEventsRequest{SandboxID: "sb-1", Tail: true})
	if err != nil || rsp.GetRet().GetRetCode() != errorcode.ErrorCode_Success {
		t.Fatalf("tail on a missing file must be an empty success: rsp=%+v err=%v", rsp, err)
	}
	if !strings.Contains(buf.String(), "shim req log not found") {
		t.Fatalf("tail mode must warn too: %s", buf.String())
	}
}

func TestGetSandboxEvents_ExistingFileDoesNotWarn(t *testing.T) {
	resetMissingLogWarn(t)
	buf := captureLog(t)
	path := filepath.Join(t.TempDir(), "cube-shim-req.log")
	writeShimLogLines(t, path, []string{shimLine("sb-1", "2026-07-22T10:00:00Z", "ok")})
	t.Setenv("CUBELET_SHIM_REQ_LOG", path)

	if _, err := (&service{}).GetSandboxEvents(context.Background(), &cubebox.GetSandboxEventsRequest{SandboxID: "sb-1"}); err != nil {
		t.Fatalf("GetSandboxEvents: %v", err)
	}
	if strings.Contains(buf.String(), "not found") || strings.Contains(buf.String(), "failed") {
		t.Fatalf("a readable log must not log a fault: %s", buf.String())
	}
}

func TestGetSandboxEvents_UnreadablePathIsLoggedAsError(t *testing.T) {
	buf := captureLog(t)
	// A directory opens but cannot be read: a non-ENOENT failure that used to be silent.
	t.Setenv("CUBELET_SHIM_REQ_LOG", t.TempDir())

	rsp, err := (&service{}).GetSandboxEvents(context.Background(), &cubebox.GetSandboxEventsRequest{SandboxID: "sb-1"})
	if err != nil {
		t.Fatalf("GetSandboxEvents: %v", err)
	}
	if rsp.GetRet().GetRetCode() != errorcode.ErrorCode_Unknown {
		t.Fatalf("want Unknown for an unreadable path, got %d", rsp.GetRet().GetRetCode())
	}
	if !strings.Contains(buf.String(), "read shim req log failed") || !strings.Contains(buf.String(), `"LogLevel":"ERROR"`) {
		t.Fatalf("an unreadable path must be logged at ERROR: %s", buf.String())
	}
}

func TestGetSandboxEvents_CancelledContextIsNotLoggedAsFault(t *testing.T) {
	buf := captureLog(t)
	path := filepath.Join(t.TempDir(), "cube-shim-req.log")
	writeShimLogLines(t, path, []string{shimLine("sb-1", "2026-07-22T10:00:00Z", "x")})
	t.Setenv("CUBELET_SHIM_REQ_LOG", path)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rsp, err := (&service{}).GetSandboxEvents(ctx, &cubebox.GetSandboxEventsRequest{SandboxID: "sb-1", Tail: true})
	if err != nil {
		t.Fatalf("GetSandboxEvents: %v", err)
	}
	if rsp.GetRet().GetRetCode() != errorcode.ErrorCode_Unknown {
		t.Fatalf("a cancelled read still reports failure, got %d", rsp.GetRet().GetRetCode())
	}
	if strings.Contains(buf.String(), "read shim req log failed") {
		t.Fatalf("a caller cancellation is not a node fault: %s", buf.String())
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
		if _, _, ok := parseShimEvent([]byte(shimLine("sb-1", ts, "x")), "sb-1"); ok {
			t.Fatalf("want invalid timestamp %q skipped", ts)
		}
	}
}

func TestParseShimEvent_SkipsOtherSandbox(t *testing.T) {
	line := []byte(shimLine("sb-other", "2026-07-22T10:00:00Z", "x"))
	if _, _, ok := parseShimEvent(line, "sb-1"); ok {
		t.Fatal("want other sandbox line skipped")
	}
}

func TestParseShimEvent_NormalizesTimestampUTC(t *testing.T) {
	line := []byte(shimLine("sb-1", "2026-07-22T10:00:00+08:00", "x"))
	ev, tsMs, ok := parseShimEvent(line, "sb-1")
	if !ok {
		t.Fatal("want valid timestamp accepted")
	}
	if want := "2026-07-22T02:00:00Z"; ev.GetTimestamp() != want {
		t.Fatalf("want normalized %q, got %q", want, ev.GetTimestamp())
	}
	if want := time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC).UnixMilli(); tsMs != want {
		t.Fatalf("want tsMs %d, got %d", want, tsMs)
	}
}

// ── Forward (cursor) mode ────────────────────────────────────────────────

func TestReadShimEventsAfter_PaginatesLikeLegacyReader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cube-shim-req.log")

	var lines []string
	for i := range 5 {
		ts := time.Date(2026, 7, 22, 10, 0, i, 0, time.UTC).Format(time.RFC3339Nano)
		lines = append(lines,
			shimLine("sb-o", ts, fmt.Sprintf("noise-%d", i)),
			shimLine("sb-page", ts, fmt.Sprintf("e%d", i)),
		)
	}
	writeShimLogLines(t, path, lines)

	// Walk pages starting from the beginning, passing back the composite cursor.
	var got []string
	var cursor eventCursor
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatal("pagination did not terminate")
		}
		events, next, hasMore, err := readShimEventsAfter(context.Background(), path, "sb-page", cursor, 2)
		if err != nil {
			t.Fatalf("readShimEventsAfter: %v", err)
		}
		for _, e := range events {
			got = append(got, e.GetMessage())
		}
		if !hasMore {
			break
		}
		if next.ms == 0 {
			t.Fatal("hasMore without nextCursor")
		}
		cursor = next
	}

	if want := "e0,e1,e2,e3,e4"; strings.Join(got, ",") != want {
		t.Fatalf("want %s, got %v", want, got)
	}
}

func TestReadShimEventsAfter_EmptyResultKeepsZeroCursor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cube-shim-req.log")
	writeShimLogLines(t, path, []string{
		shimLine("sb-o", "2026-07-22T10:00:00Z", "other"),
	})

	events, next, hasMore, err := readShimEventsAfter(context.Background(), path, "sb-none", eventCursor{}, 10)
	if err != nil {
		t.Fatalf("readShimEventsAfter: %v", err)
	}
	if len(events) != 0 || next != (eventCursor{}) || hasMore {
		t.Fatalf("want empty first page, got %d events next=%+v hasMore=%v", len(events), next, hasMore)
	}
}

func TestGetSandboxEvents_ForwardModePagesFromBeginning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cube-shim-req.log")
	writeShimLogLines(t, path, []string{
		shimLine("sb-f", "2026-07-22T10:00:00Z", "e0"),
		shimLine("sb-f", "2026-07-22T10:00:01Z", "e1"),
		shimLine("sb-f", "2026-07-22T10:00:02Z", "e2"),
	})

	t.Setenv("CUBELET_SHIM_REQ_LOG", path)
	s := &service{}

	page1, err := s.GetSandboxEvents(context.Background(), &cubebox.GetSandboxEventsRequest{
		SandboxID: "sb-f",
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("GetSandboxEvents: %v", err)
	}
	if len(page1.GetEvents()) != 2 ||
		page1.GetEvents()[0].GetMessage() != "e0" ||
		page1.GetEvents()[1].GetMessage() != "e1" {
		t.Fatalf("want first page from beginning, got %+v", page1.GetEvents())
	}
	if !page1.GetHasMore() {
		t.Fatal("want hasMore on full first page")
	}

	page2, err := s.GetSandboxEvents(context.Background(), &cubebox.GetSandboxEventsRequest{
		SandboxID: "sb-f",
		Cursor:    page1.GetNextCursor(),
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("GetSandboxEvents: %v", err)
	}
	if len(page2.GetEvents()) != 1 || page2.GetEvents()[0].GetMessage() != "e2" {
		t.Fatalf("want remainder page, got %+v", page2.GetEvents())
	}
	if page2.GetHasMore() {
		t.Fatal("want hasMore=false on last page")
	}
}

func TestGetSandboxEvents_TailModeReturnsNewestWithNextCursor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cube-shim-req.log")
	writeShimLogLines(t, path, []string{
		shimLine("sb-t", "2026-07-22T10:00:00Z", "old"),
		shimLine("sb-t", "2026-07-22T10:00:01Z", "mid"),
		shimLine("sb-t", "2026-07-22T10:00:02Z", "new"),
	})

	t.Setenv("CUBELET_SHIM_REQ_LOG", path)
	s := &service{}
	rsp, err := s.GetSandboxEvents(context.Background(), &cubebox.GetSandboxEventsRequest{
		SandboxID: "sb-t",
		Tail:      true,
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("GetSandboxEvents: %v", err)
	}
	if rsp.GetRet().GetRetCode() != errorcode.ErrorCode_Success {
		t.Fatalf("want success, got %d", rsp.GetRet().GetRetCode())
	}
	if len(rsp.GetEvents()) != 2 ||
		rsp.GetEvents()[0].GetMessage() != "mid" ||
		rsp.GetEvents()[1].GetMessage() != "new" {
		t.Fatalf("want newest two oldest-first, got %+v", rsp.GetEvents())
	}
	if want := time.Date(2026, 7, 22, 10, 0, 2, 0, time.UTC).UnixMilli(); rsp.GetNextCursor() != want {
		t.Fatalf("want nextCursor %d, got %d", want, rsp.GetNextCursor())
	}
	if rsp.GetHasMore() {
		t.Fatal("tail mode is the last page; want hasMore=false")
	}
}

func TestGetSandboxEvents_TailAndCursorRejected(t *testing.T) {
	s := &service{}
	rsp, err := s.GetSandboxEvents(context.Background(), &cubebox.GetSandboxEventsRequest{
		SandboxID: "sb-x",
		Tail:      true,
		Cursor:    123,
	})
	if err != nil {
		t.Fatalf("GetSandboxEvents: %v", err)
	}
	if rsp.GetRet().GetRetCode() != errorcode.ErrorCode_InvalidParamFormat {
		t.Fatalf("want InvalidParamFormat, got %d", rsp.GetRet().GetRetCode())
	}
}

// ── Composite cursor (ms + same-ms seq) ──────────────────────────────────

// sameMsLog writes n sandbox events at one ms followed by tailTs events at later seconds.
func sameMsLog(t *testing.T, path, id string, n int, later ...string) {
	t.Helper()
	var lines []string
	for i := range n {
		ts := fmt.Sprintf("2026-07-22T10:00:00.123%06dZ", i*1000)
		lines = append(lines, shimLine(id, ts, fmt.Sprintf("e%d", i)))
	}
	for i, ts := range later {
		lines = append(lines, shimLine(id, ts, fmt.Sprintf("l%d", i)))
	}
	writeShimLogLines(t, path, lines)
}

// walkEvents pages through the log with the composite cursor and returns all messages.
func walkEvents(t *testing.T, path, id string, limit int32) []string {
	t.Helper()
	var got []string
	var cur eventCursor
	for page := 0; ; page++ {
		if page > 100 {
			t.Fatal("pagination did not terminate")
		}
		events, next, hasMore, err := readShimEventsAfter(context.Background(), path, id, cur, limit)
		if err != nil {
			t.Fatalf("readShimEventsAfter: %v", err)
		}
		if int32(len(events)) > limit {
			t.Fatalf("page has %d events, over limit %d", len(events), limit)
		}
		for _, e := range events {
			got = append(got, e.GetMessage())
		}
		if !hasMore {
			return got
		}
		cur = next
	}
}

func TestReadShimEventsAfter_SameMillisecondAcrossPages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cube-shim-req.log")
	sameMsLog(t, path, "sb-1", 5, "2026-07-22T10:00:01Z")

	for _, limit := range []int32{1, 2, 3, 5, 6} {
		got := walkEvents(t, path, "sb-1", limit)
		if want := "e0,e1,e2,e3,e4,l0"; strings.Join(got, ",") != want {
			t.Fatalf("limit=%d: want %s, got %v", limit, want, got)
		}
	}
}

func TestReadShimEventsAfter_AllEventsInOneMillisecond(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cube-shim-req.log")
	sameMsLog(t, path, "sb-1", 4)

	if got := walkEvents(t, path, "sb-1", 1); strings.Join(got, ",") != "e0,e1,e2,e3" {
		t.Fatalf("want every event exactly once, got %v", got)
	}
}

func TestReadShimEventsAfter_SeqIgnoresOtherSandboxesAndBadLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cube-shim-req.log")
	writeShimLogLines(t, path, []string{
		shimLine("sb-1", "2026-07-22T10:00:00.123000000Z", "a"),
		shimLine("sb-other", "2026-07-22T10:00:00.123100000Z", "noise"),
		"not json",
		shimLine("sb-1", "bad-time", "bad"),
		shimLine("sb-1", "2026-07-22T10:00:00.123200000Z", "b"),
		shimLine("sb-1", "2026-07-22T10:00:00.123300000Z", "c"),
	})

	if got := walkEvents(t, path, "sb-1", 1); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("want a,b,c, got %v", got)
	}
}

func TestReadShimEventsAfter_SeqZeroIsPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cube-shim-req.log")
	writeShimLogLines(t, path, []string{
		shimLine("sb-1", "2026-07-22T10:00:00Z", "e0"),
		shimLine("sb-1", "2026-07-22T10:00:01Z", "e1"),
	})

	events, next, hasMore, err := readShimEventsAfter(context.Background(), path, "sb-1", eventCursor{}, 1)
	if err != nil || len(events) != 1 || !hasMore {
		t.Fatalf("first page: events=%d hasMore=%v err=%v", len(events), hasMore, err)
	}
	if next.seq != 0 || !next.hasSeq {
		t.Fatalf("want seq 0 present, got %+v", next)
	}
	rest, _, _, err := readShimEventsAfter(context.Background(), path, "sb-1", next, 10)
	if err != nil || len(rest) != 1 || rest[0].GetMessage() != "e1" {
		t.Fatalf("seq 0 cursor must resume at e1, got %+v err=%v", rest, err)
	}
}

func TestReadShimEventsAfter_LegacyCursorSkipsWholeMillisecond(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cube-shim-req.log")
	sameMsLog(t, path, "sb-1", 3, "2026-07-22T10:00:01Z")

	ms := time.Date(2026, 7, 22, 10, 0, 0, 123000000, time.UTC).UnixMilli()
	events, _, _, err := readShimEventsAfter(context.Background(), path, "sb-1", eventCursor{ms: ms}, 10)
	if err != nil {
		t.Fatalf("readShimEventsAfter: %v", err)
	}
	if len(events) != 1 || events[0].GetMessage() != "l0" {
		t.Fatalf("legacy cursor-only must keep exclusive-ms semantics, got %+v", events)
	}
}

func TestScanShimEventsNewest_SeqCountsWholeNewestMillisecond(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cube-shim-req.log")
	writeShimLogLines(t, path, []string{
		shimLine("sb-1", "2026-07-22T10:00:00.100000000Z", "old"),
		shimLine("sb-1", "2026-07-22T10:00:00.123000000Z", "a"),
		shimLine("sb-other", "2026-07-22T10:00:00.123100000Z", "noise"),
		shimLine("sb-1", "2026-07-22T10:00:00.123200000Z", "b"),
		shimLine("sb-1", "2026-07-22T10:00:00.123300000Z", "c"),
	})

	// limit=2 returns b,c but the cursor must index c within the whole ms run (a,b,c).
	events, newest, err := readShimEventsNewest(context.Background(), path, "sb-1", 2)
	if err != nil {
		t.Fatalf("readShimEventsNewest: %v", err)
	}
	if len(events) != 2 || events[0].GetMessage() != "b" || events[1].GetMessage() != "c" {
		t.Fatalf("want b,c, got %+v", events)
	}
	if newest.seq != 2 || !newest.hasSeq {
		t.Fatalf("want newest seq 2, got %+v", newest)
	}
}

func TestGetSandboxEvents_TailThenFollowSameMillisecond(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cube-shim-req.log")
	writeShimLogLines(t, path, []string{
		shimLine("sb-1", "2026-07-22T10:00:00.123000000Z", "e0"),
		shimLine("sb-1", "2026-07-22T10:00:00.123100000Z", "e1"),
	})
	t.Setenv("CUBELET_SHIM_REQ_LOG", path)
	s := &service{}

	tail, err := s.GetSandboxEvents(context.Background(), &cubebox.GetSandboxEventsRequest{
		SandboxID: "sb-1", Tail: true, Limit: 10,
	})
	if err != nil || tail.GetRet().GetRetCode() != errorcode.ErrorCode_Success {
		t.Fatalf("tail: rsp=%+v err=%v", tail, err)
	}
	if tail.GetNextCursorSeq() != 1 {
		t.Fatalf("want tail seq 1, got %d", tail.GetNextCursorSeq())
	}

	// An event lands in the same ms after the tail read.
	writeShimLogLines(t, path, []string{shimLine("sb-1", "2026-07-22T10:00:00.123200000Z", "e2")})

	seq := tail.GetNextCursorSeq()
	follow, err := s.GetSandboxEvents(context.Background(), &cubebox.GetSandboxEventsRequest{
		SandboxID: "sb-1", Cursor: tail.GetNextCursor(), CursorSeq: &seq,
	})
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	if len(follow.GetEvents()) != 1 || follow.GetEvents()[0].GetMessage() != "e2" {
		t.Fatalf("want only the new same-ms event, got %+v", follow.GetEvents())
	}
}

func TestGetSandboxEvents_RejectsInvalidCursorSeq(t *testing.T) {
	neg, zero := int64(-1), int64(0)
	cases := map[string]*cubebox.GetSandboxEventsRequest{
		"negative seq":         {SandboxID: "sb-1", Cursor: 100, CursorSeq: &neg},
		"seq without cursor":   {SandboxID: "sb-1", CursorSeq: &zero},
		"tail with seq":        {SandboxID: "sb-1", Tail: true, CursorSeq: &zero},
		"tail with cursor":     {SandboxID: "sb-1", Tail: true, Cursor: 100},
		"seq with zero cursor": {SandboxID: "sb-1", Cursor: 0, CursorSeq: &zero},
	}
	s := &service{}
	for name, req := range cases {
		rsp, err := s.GetSandboxEvents(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if rsp.GetRet().GetRetCode() != errorcode.ErrorCode_InvalidParamFormat {
			t.Fatalf("%s: want InvalidParamFormat, got %d", name, rsp.GetRet().GetRetCode())
		}
	}
}

// ── Tail run spanning the scan window ────────────────────────────────────

// minOffsetReaderAt records the lowest offset read to show how far a scan went back.
type minOffsetReaderAt struct {
	data   []byte
	minOff int64
}

func (r *minOffsetReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if r.minOff < 0 || off < r.minOff {
		r.minOff = off
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// bigRunLog writes count same-ms events whose total size exceeds the scan window.
func bigRunLog(t *testing.T, path, id string) (count int, pad string) {
	t.Helper()
	pad = strings.Repeat("x", 512*1024)
	count = int(maxScanBytes/int64(len(pad))) + 8
	lines := make([]string, count)
	for i := range lines {
		lines[i] = shimLine(id, fmt.Sprintf("2026-07-22T10:00:00.123%06dZ", i*1000), pad)
	}
	writeShimLogLines(t, path, lines)
	return count, pad
}

func TestReadShimEventsNewest_RunSpanningScanWindowCountsInsideWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cube-shim-req.log")
	count, _ := bigRunLog(t, path, "sb-1")

	events, newest, err := readShimEventsNewest(context.Background(), path, "sb-1", 1)
	if err != nil {
		t.Fatalf("readShimEventsNewest: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if trueSeq := int64(count - 1); newest.seq <= 0 || newest.seq >= trueSeq {
		t.Fatalf("want a seq counted inside the window below %d, got %d", trueSeq, newest.seq)
	}

	// The undercounted seq may replay the cut part of the run but never skip an event.
	replay := int64(count-1) - newest.seq
	after, _, _, err := readShimEventsAfter(context.Background(), path, "sb-1", newest, 100)
	if err != nil || int64(len(after)) != replay {
		t.Fatalf("want %d replayed events, got %d err=%v", replay, len(after), err)
	}
	writeShimLogLines(t, path, []string{shimLine("sb-1", "2026-07-22T10:00:00.123999000Z", "late")})
	after, _, _, err = readShimEventsAfter(context.Background(), path, "sb-1", newest, 100)
	if err != nil || int64(len(after)) != replay+1 || after[len(after)-1].GetMessage() != "late" {
		t.Fatalf("want the late event last among %d, got %d err=%v", replay+1, len(after), err)
	}
}

func TestScanShimEventsNewest_ScanIsBoundedByWindow(t *testing.T) {
	var base bytes.Buffer
	noise := shimLine("sb-other", "2026-07-22T10:00:00.100000000Z", strings.Repeat("o", 200))
	for int64(base.Len()) < 3*maxScanBytes {
		base.WriteString(noise + "\n")
	}
	ms := func(i int) string { return fmt.Sprintf("2026-07-22T10:00:09.123%06dZ", i*1000) }
	cases := []struct {
		name       string
		tail       []string
		wantEvents int
		wantSeq    int64
	}{
		{"no match", nil, 0, 0},
		{"single match", []string{shimLine("sb-1", ms(0), "a")}, 1, 0},
		{"same-ms matches", []string{shimLine("sb-1", ms(0), "a"), shimLine("sb-1", ms(1), "b"), shimLine("sb-1", ms(2), "c")}, 3, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := append(base.Bytes()[:base.Len():base.Len()], strings.Join(append(c.tail, ""), "\n")...)
			r := &minOffsetReaderAt{data: data, minOff: -1}

			events, newest, err := scanShimEventsNewest(context.Background(), r, int64(len(data)), "sb-1", 5)
			if err != nil {
				t.Fatalf("scanShimEventsNewest: %v", err)
			}
			if len(events) != c.wantEvents || newest.seq != c.wantSeq {
				t.Fatalf("want %d events seq %d, got %d seq %d", c.wantEvents, c.wantSeq, len(events), newest.seq)
			}
			if back := int64(len(data)) - r.minOff; back > maxScanBytes {
				t.Fatalf("scan read %d bytes back, want at most %d", back, maxScanBytes)
			}
		})
	}
}

func TestScanShimEventsNewest_ClosedRunStaysInsideScanWindow(t *testing.T) {
	var b bytes.Buffer
	old := shimLine("sb-1", "2026-07-22T10:00:00.100000000Z", strings.Repeat("o", 200))
	for int64(b.Len()) < maxScanBytes+scannerBufSize {
		b.WriteString(old + "\n")
	}
	for i := range 3 {
		b.WriteString(shimLine("sb-1", fmt.Sprintf("2026-07-22T10:00:09.123%06dZ", i*1000), "new") + "\n")
	}
	r := &minOffsetReaderAt{data: b.Bytes(), minOff: -1}

	_, newest, err := scanShimEventsNewest(context.Background(), r, int64(b.Len()), "sb-1", 2)
	if err != nil {
		t.Fatalf("scanShimEventsNewest: %v", err)
	}
	if newest.seq != 2 {
		t.Fatalf("want seq 2, got %d", newest.seq)
	}
	if back := int64(b.Len()) - r.minOff; back > maxScanBytes {
		t.Fatalf("a closed run must not pass the window: scanned %d bytes back", back)
	}
}

func TestReadShimEventsNewest_RunSpanningScanWindowHonorsContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cube-shim-req.log")
	bigRunLog(t, path, "sb-1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := readShimEventsNewest(ctx, path, "sb-1", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// ── Tail scan robustness ─────────────────────────────────────────────────

// truncatingReaderAt simulates a log that shrinks mid-scan: reads after the
// first one only see data up to truncTo.
type truncatingReaderAt struct {
	data    []byte
	truncTo int64
	calls   int
}

func (r *truncatingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.calls++
	end := int64(len(r.data))
	if r.calls > 1 {
		end = r.truncTo
	}
	if off >= end {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:end])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestScanShimEventsNewest_TruncatedMidScanKeepsEvents(t *testing.T) {
	pad := shimLine("sb-o", "2026-07-22T10:00:00Z", strings.Repeat("x", 64))
	target := shimLine("sb-tr", "2026-07-22T10:00:01Z", "kept")

	var data []byte
	for len(data) < scannerBufSize+scannerBufSize/2 {
		data = append(data, []byte(pad+"\n")...)
	}
	data = append(data, []byte(target+"\n")...)
	for i := 0; i < 8; i++ {
		data = append(data, []byte(pad+"\n")...)
	}

	// The first read covers the newest chunk (contains target); later reads
	// hit a file truncated to a quarter chunk of padding only.
	r := &truncatingReaderAt{data: data, truncTo: int64(scannerBufSize / 4)}

	events, _, err := scanShimEventsNewest(context.Background(), r, int64(len(data)), "sb-tr", 10)
	if err != nil {
		t.Fatalf("scanShimEventsNewest: %v", err)
	}
	if len(events) != 1 || events[0].GetMessage() != "kept" {
		t.Fatalf("want the event from the first chunk kept, got %+v", events)
	}
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"context"
	"fmt"
	"io"
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

	// Walk pages starting from cursor 0 (beginning) like the legacy reader.
	var got []string
	var cursor int64
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
		if next == 0 {
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

	events, next, hasMore, err := readShimEventsAfter(context.Background(), path, "sb-none", 0, 10)
	if err != nil {
		t.Fatalf("readShimEventsAfter: %v", err)
	}
	if len(events) != 0 || next != 0 || hasMore {
		t.Fatalf("want empty first page, got %d events next=%d hasMore=%v", len(events), next, hasMore)
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

	events, err := scanShimEventsNewest(context.Background(), r, int64(len(data)), "sb-tr", 10)
	if err != nil {
		t.Fatalf("scanShimEventsNewest: %v", err)
	}
	if len(events) != 1 || events[0].GetMessage() != "kept" {
		t.Fatalf("want the event from the first chunk kept, got %+v", events)
	}
}

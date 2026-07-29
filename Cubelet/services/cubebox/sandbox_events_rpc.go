// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	jsoniter "github.com/json-iterator/go"
	"golang.org/x/time/rate"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/pkgs/proto/services/errorcode/v1"
)

const (
	// defaultShimReqLogPath is CubeShim's node-local JSON log, shared by all
	// sandboxes on the node. Keep in sync with CubeShim/shim/src/log/mod.rs
	// LOG_DIR + LOG_FILE.
	defaultShimReqLogPath = "/data/log/CubeShim/cube-shim-req.log"

	// defaultEventLimit / maxEventLimit mirror CubeMaster's sandbox_logs.go
	// constants.
	defaultEventLimit = 200
	maxEventLimit     = 2000

	// scannerBufSize caps a log line buffer; forwarded stderr can be large.
	scannerBufSize = 256 * 1024

	// maxScanBytes caps the tail scan per request to avoid a full-file scan.
	maxScanBytes int64 = 16 * 1024 * 1024
)

// missingLogWarn limits the missing-shim-log warning to one per interval per node.
var missingLogWarn = &rate.Sometimes{Interval: 5 * time.Minute}

// eventCursor locates an event by Unix-ms ts plus its index among this
// sandbox's consecutive same-ms events; hasSeq=false is the ms-only legacy cursor.
type eventCursor struct {
	ms     int64
	seq    int64
	hasSeq bool
}

// shimLogLine mirrors CubeShim's LogItem (PascalCase JSON). Only needed fields
// are decoded; LogContent can be arbitrarily large.
type shimLogLine struct {
	Module       string `json:"Module"`
	InstanceID   string `json:"InstanceId"`
	ContainerID  string `json:"ContainerId"`
	Timestamp    string `json:"Timestamp"`
	LogContent   string `json:"LogContent"`
	FunctionType string `json:"FunctionType"`
}

// parseShimEvent decodes one log line into an event plus its Unix-ms ts,
// skipping non-matching or invalid lines.
func parseShimEvent(line []byte, sandboxID string) (*cubebox.SandboxEvent, int64, bool) {
	var entry shimLogLine
	if err := jsoniter.Unmarshal(line, &entry); err != nil || entry.InstanceID != sandboxID {
		return nil, 0, false
	}
	ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
	if err != nil {
		return nil, 0, false
	}
	return &cubebox.SandboxEvent{
		Timestamp: ts.UTC().Format(time.RFC3339Nano),
		Level:     "info",
		Message:   entry.LogContent,
		Module:    entry.Module,
	}, ts.UnixMilli(), true
}

// GetSandboxEvents returns sandbox events, newest-first in tail mode or paged from the beginning.
func (s *service) GetSandboxEvents(ctx context.Context, req *cubebox.GetSandboxEventsRequest) (*cubebox.GetSandboxEventsResponse, error) {
	rsp := &cubebox.GetSandboxEventsResponse{
		RequestID: req.GetRequestID(),
		Ret:       &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}

	sandboxID := strings.TrimSpace(req.GetSandboxID())
	if sandboxID == "" {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = "sandboxID is required"
		return rsp, nil
	}
	if err := pathutil.ValidateSafeID(sandboxID); err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = "invalid sandboxID: " + err.Error()
		return rsp, nil
	}
	if req.GetTail() && (req.GetCursor() > 0 || req.CursorSeq != nil) {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = "tail and cursor are mutually exclusive"
		return rsp, nil
	}
	if req.CursorSeq != nil && (req.GetCursorSeq() < 0 || req.GetCursor() <= 0) {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = "cursorSeq must be >= 0 and requires cursor"
		return rsp, nil
	}

	limit := req.GetLimit()
	if limit <= 0 {
		limit = defaultEventLimit
	}
	if limit > maxEventLimit {
		limit = maxEventLimit
	}

	logPath := shimReqLogPath()
	if req.GetTail() {
		events, newest, err := readShimEventsNewest(ctx, logPath, sandboxID, limit)
		if !fillEventsFromRead(ctx, rsp, logPath, err) {
			return rsp, nil
		}
		rsp.Events, rsp.NextCursor, rsp.NextCursorSeq = events, newest.ms, newest.seq
		return rsp, nil
	}

	after := eventCursor{ms: req.GetCursor()}
	if req.CursorSeq != nil {
		after.seq, after.hasSeq = req.GetCursorSeq(), true
	}
	events, last, hasMore, err := readShimEventsAfter(ctx, logPath, sandboxID, after, limit)
	if !fillEventsFromRead(ctx, rsp, logPath, err) {
		return rsp, nil
	}
	rsp.Events, rsp.NextCursor, rsp.NextCursorSeq, rsp.HasMore = events, last.ms, last.seq, hasMore
	return rsp, nil
}

// fillEventsFromRead maps a read error onto rsp; a missing file means no events yet but is
// logged, as it also hides an unreadable path. Returns false when rsp is final.
func fillEventsFromRead(ctx context.Context, rsp *cubebox.GetSandboxEventsResponse, path string, err error) bool {
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		missingLogWarn.Do(func() {
			log.G(ctx).Warnf("shim req log not found, returning no events: path=%s err=%v", path, err)
		})
		rsp.Events = []*cubebox.SandboxEvent{}
		return false
	}
	// A cancelled or timed-out caller is gone, not a node fault.
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		log.G(ctx).Errorf("read shim req log failed: path=%s err=%v", path, err)
	}
	rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
	rsp.Ret.RetMsg = "failed to read shim log"
	return false
}

// shimReqLogPath honours the CUBELET_SHIM_REQ_LOG env override (for tests /
// non-standard installs) and falls back to defaultShimReqLogPath.
func shimReqLogPath() string {
	if p := strings.TrimSpace(os.Getenv("CUBELET_SHIM_REQ_LOG")); p != "" {
		return p
	}
	return defaultShimReqLogPath
}

// readShimEventsNewest returns up to limit newest events, oldest first, and the newest event's cursor.
func readShimEventsNewest(ctx context.Context, logPath, sandboxID string, limit int32) ([]*cubebox.SandboxEvent, eventCursor, error) {
	f, err := os.Open(filepath.Clean(logPath))
	if err != nil {
		return nil, eventCursor{}, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, eventCursor{}, err
	}
	return scanShimEventsNewest(ctx, f, info.Size(), sandboxID, limit)
}

// scanLinesReverse feeds visit the complete lines newest first until visit returns false.
// It never reads before the last maxScanBytes; a line cut by that bound is dropped.
func scanLinesReverse(ctx context.Context, r io.ReaderAt, size int64, visit func(line []byte) bool) error {
	// Reverse-scan in chunks, keeping a partial line until earlier bytes arrive.
	buf := make([]byte, scannerBufSize)
	var tail []byte
	readPos := size
	scanFloor := max(size-maxScanBytes, 0)

	for readPos > scanFloor {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunkSize := min(int64(len(buf)), readPos-scanFloor)
		start := readPos - chunkSize
		n, err := r.ReadAt(buf[:chunkSize], start)
		if n > 0 {
			readPos = start

			// Prepend this chunk's bytes to tail (keep file order).
			newTail := make([]byte, 0, int64(n)+int64(len(tail)))
			newTail = append(newTail, buf[:n]...)
			newTail = append(newTail, tail...)
			tail = newTail

			// Cut complete lines from the newest end of tail until none remains.
			for {
				nl := bytes.LastIndexByte(tail, '\n')
				if nl < 0 {
					break // incomplete line; need earlier bytes
				}
				line := tail[nl+1:]
				tail = tail[:nl]
				if len(line) > 0 && !visit(line) {
					return nil
				}
			}
		}
		if err != nil {
			// Log shrank mid-scan (rotation/truncation); keep what we have.
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
	}

	// At the file start the leftover is the oldest complete line; elsewhere it is cut.
	if readPos == 0 && len(tail) > 0 {
		visit(tail)
	}
	return nil
}

// scanShimEventsNewest returns up to limit newest events, oldest first. The cursor seq counts the
// newest ms run to its start, so the scan may pass limit but never the maxScanBytes window.
func scanShimEventsNewest(ctx context.Context, r io.ReaderAt, size int64, sandboxID string, limit int32) ([]*cubebox.SandboxEvent, eventCursor, error) {
	events := make([]*cubebox.SandboxEvent, 0, limit)
	var newest eventCursor
	var run int64
	runOpen := true

	err := scanLinesReverse(ctx, r, size, func(line []byte) bool {
		ev, ms, ok := parseShimEvent(line, sandboxID)
		if !ok {
			return true
		}
		if len(events) == 0 {
			newest = eventCursor{ms: ms, hasSeq: true}
		}
		if runOpen {
			if ms == newest.ms {
				run++
			} else {
				runOpen = false
			}
		}
		if int32(len(events)) < limit {
			events = append(events, ev)
		}
		return int32(len(events)) < limit || runOpen
	})

	// events were collected newest-first; reverse to oldest-first.
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	if run > 0 {
		newest.seq = run - 1
	}
	return events, newest, err
}

// readShimEventsAfter scans forward from the beginning, returning up to limit events after
// the cursor, oldest first; hasMore marks a further page, last is the final event's cursor.
func readShimEventsAfter(ctx context.Context, logPath, sandboxID string, after eventCursor, limit int32) ([]*cubebox.SandboxEvent, eventCursor, bool, error) {
	f, err := os.Open(filepath.Clean(logPath))
	if err != nil {
		return nil, eventCursor{}, false, err
	}
	defer f.Close()

	events := make([]*cubebox.SandboxEvent, 0, limit)
	var last eventCursor
	hasMore := false
	var prevMs, seq int64
	seen := false

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, scannerBufSize), int(maxScanBytes))

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return events, last, hasMore, err
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		ev, ms, ok := parseShimEvent(line, sandboxID)
		if !ok {
			continue
		}
		// Count before the cursor filter so skipped events keep their seq.
		if seen && ms == prevMs {
			seq++
		} else {
			seq = 0
		}
		prevMs, seen = ms, true
		if after.ms > 0 && (ms < after.ms || (ms == after.ms && (!after.hasSeq || seq <= after.seq))) {
			continue
		}
		if int32(len(events)) >= limit {
			hasMore = true
			break
		}
		events = append(events, ev)
		last = eventCursor{ms: ms, seq: seq, hasSeq: true}
	}
	if err := scanner.Err(); err != nil {
		return events, last, hasMore, err
	}
	return events, last, hasMore, nil
}

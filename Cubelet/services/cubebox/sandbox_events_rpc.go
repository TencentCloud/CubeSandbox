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
	if req.GetTail() && req.GetCursor() > 0 {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = "tail and cursor are mutually exclusive"
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
		events, err := readShimEventsNewest(ctx, logPath, sandboxID, limit)
		if !fillEventsFromRead(rsp, err) {
			return rsp, nil
		}
		rsp.Events = events
		rsp.NextCursor = newestEventCursorMillis(events)
		return rsp, nil
	}

	events, nextCursor, hasMore, err := readShimEventsAfter(ctx, logPath, sandboxID, req.GetCursor(), limit)
	if !fillEventsFromRead(rsp, err) {
		return rsp, nil
	}
	rsp.Events, rsp.NextCursor, rsp.HasMore = events, nextCursor, hasMore
	return rsp, nil
}

// fillEventsFromRead maps a read error onto rsp; a missing file means no
// events yet. Returns false when rsp is final.
func fillEventsFromRead(rsp *cubebox.GetSandboxEventsResponse, err error) bool {
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		rsp.Events = []*cubebox.SandboxEvent{}
		return false
	}
	rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
	rsp.Ret.RetMsg = "failed to read shim log"
	return false
}

// newestEventCursorMillis returns the newest event's Unix-ms ts for tail-mode followers.
func newestEventCursorMillis(events []*cubebox.SandboxEvent) int64 {
	if n := len(events); n > 0 {
		if ts, err := time.Parse(time.RFC3339Nano, events[n-1].GetTimestamp()); err == nil {
			return ts.UnixMilli()
		}
	}
	return 0
}

// shimReqLogPath honours the CUBELET_SHIM_REQ_LOG env override (for tests /
// non-standard installs) and falls back to defaultShimReqLogPath.
func shimReqLogPath() string {
	if p := strings.TrimSpace(os.Getenv("CUBELET_SHIM_REQ_LOG")); p != "" {
		return p
	}
	return defaultShimReqLogPath
}

// readShimEventsNewest returns up to limit newest events, oldest first.
func readShimEventsNewest(ctx context.Context, logPath, sandboxID string, limit int32) ([]*cubebox.SandboxEvent, error) {
	f, err := os.Open(filepath.Clean(logPath))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return scanShimEventsNewest(ctx, f, info.Size(), sandboxID, limit)
}

// scanShimEventsNewest reverse-scans the last maxScanBytes, returning up to limit newest events, oldest first.
func scanShimEventsNewest(ctx context.Context, r io.ReaderAt, size int64, sandboxID string, limit int32) ([]*cubebox.SandboxEvent, error) {
	if size == 0 {
		return []*cubebox.SandboxEvent{}, nil
	}

	// Reverse-scan in chunks, keeping a partial line until earlier bytes arrive.
	buf := make([]byte, scannerBufSize)
	var tail []byte
	readPos := size
	scanFloor := max(size-maxScanBytes, 0)
	events := make([]*cubebox.SandboxEvent, 0, limit)

	for readPos > scanFloor && int32(len(events)) < limit {
		if err := ctx.Err(); err != nil {
			return events, err
		}
		chunkSize := min(int64(len(buf)), readPos)
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
			for int32(len(events)) < limit {
				nl := bytes.LastIndexByte(tail, '\n')
				if nl < 0 {
					break // incomplete line; need earlier bytes
				}
				line := tail[nl+1:]
				tail = tail[:nl]
				if len(line) == 0 {
					continue
				}
				if ev, _, ok := parseShimEvent(line, sandboxID); ok {
					events = append(events, ev)
				}
			}
		}
		if err != nil {
			// Log shrank mid-scan (rotation/truncation); keep what we have.
			if errors.Is(err, io.EOF) {
				break
			}
			return events, err
		}
	}

	// Leftover tail is the oldest complete line (newline already stripped).
	if int32(len(events)) < limit && len(tail) > 0 {
		if ev, _, ok := parseShimEvent(tail, sandboxID); ok {
			events = append(events, ev)
		}
	}

	// events were collected newest-first; reverse to oldest-first.
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	return events, nil
}

// readShimEventsAfter scans forward from the beginning, returning up to limit
// events newer than cursor (Unix ms), oldest first; hasMore marks a further page.
func readShimEventsAfter(ctx context.Context, logPath, sandboxID string, cursor int64, limit int32) ([]*cubebox.SandboxEvent, int64, bool, error) {
	f, err := os.Open(filepath.Clean(logPath))
	if err != nil {
		return nil, 0, false, err
	}
	defer f.Close()

	events := make([]*cubebox.SandboxEvent, 0, limit)
	var nextCursor int64
	hasMore := false

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, scannerBufSize), int(maxScanBytes))

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return events, nextCursor, hasMore, err
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		ev, tsMs, ok := parseShimEvent(line, sandboxID)
		if !ok {
			continue
		}
		if cursor > 0 && tsMs <= cursor {
			continue
		}
		if int32(len(events)) >= limit {
			hasMore = true
			break
		}
		events = append(events, ev)
		nextCursor = tsMs
	}
	if err := scanner.Err(); err != nil {
		return events, nextCursor, hasMore, err
	}
	return events, nextCursor, hasMore, nil
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

import (
	"bytes"
	"context"
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
	// defaultShimReqLogPath is CubeShim's node-local JSON log.
	// Keep in sync with CubeShim/shim/src/log/mod.rs LOG_DIR + LOG_FILE.
	defaultShimReqLogPath = "/data/log/CubeShim/cube-shim-req.log"

	// defaultEventLimit / maxEventLimit mirror CubeMaster's old sandbox_logs.go
	// constants to keep API pagination behaviour unchanged.
	defaultEventLimit = 200
	maxEventLimit     = 1000

	// scannerBufSize caps a log line buffer; forwarded stderr can be large.
	scannerBufSize = 256 * 1024

	// maxScanBytes caps the backward scan per request to avoid a full-file scan.
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

// parseShimEvent decodes one shim log line into an event, skipping lines that
// don't match sandboxID or carry an invalid timestamp.
func parseShimEvent(line []byte, sandboxID string) (*cubebox.SandboxEvent, bool) {
	var entry shimLogLine
	if err := jsoniter.Unmarshal(line, &entry); err != nil || entry.InstanceID != sandboxID {
		return nil, false
	}
	ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
	if err != nil {
		return nil, false
	}
	return &cubebox.SandboxEvent{
		Timestamp: ts.UTC().Format(time.RFC3339Nano),
		Level:     "info",
		Message:   entry.LogContent,
		Module:    entry.Module,
	}, true
}

// GetSandboxEvents returns up to limit newest events for the sandbox.
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

	limit := req.GetLimit()
	if limit <= 0 {
		limit = defaultEventLimit
	}
	if limit > maxEventLimit {
		limit = maxEventLimit
	}

	events, err := readShimEventsNewest(ctx, shimReqLogPath(), sandboxID, limit)
	if err != nil {
		// A missing file is a normal "no events yet" state, not an error.
		if os.IsNotExist(err) {
			rsp.Events = []*cubebox.SandboxEvent{}
			return rsp, nil
		}
		rsp.Ret.RetCode = errorcode.ErrorCode_Unknown
		rsp.Ret.RetMsg = "failed to read shim log"
		return rsp, nil
	}

	rsp.Events = events
	return rsp, nil
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
	cleanPath := filepath.Clean(logPath)
	f, err := os.Open(cleanPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
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
		if _, err := f.ReadAt(buf[:chunkSize], start); err != nil {
			return events, err
		}
		readPos = start

		// Prepend this chunk's bytes to tail (keep file order).
		newTail := make([]byte, 0, chunkSize+int64(len(tail)))
		newTail = append(newTail, buf[:chunkSize]...)
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
			if ev, ok := parseShimEvent(line, sandboxID); ok {
				events = append(events, ev)
			}
		}
	}

	// Leftover tail is the oldest complete line (newline already stripped).
	if int32(len(events)) < limit && len(tail) > 0 {
		if ev, ok := parseShimEvent(tail, sandboxID); ok {
			events = append(events, ev)
		}
	}

	// events were collected newest-first; reverse to oldest-first.
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	return events, nil
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubesandbox

import (
	"bufio"
	"encoding/json"
	"io"
)

func parseStream(r io.Reader, execution *Execution, opts RunCodeOptions) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		parseLine(execution, scanner.Bytes(), opts)
	}
	return scanner.Err()
}

func parseLine(execution *Execution, line []byte, opts RunCodeOptions) {
	if len(line) == 0 {
		return
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(line, &envelope); err != nil {
		return
	}

	var eventType string
	if err := json.Unmarshal(envelope["type"], &eventType); err != nil {
		return
	}

	switch eventType {
	case "result":
		var result Result
		if err := json.Unmarshal(line, &result); err != nil {
			return
		}
		execution.Results = append(execution.Results, result)
		if result.IsMainResult {
			execution.Text = result.Text
		}
		if opts.OnResult != nil {
			opts.OnResult(result)
		}
	case "stdout":
		var event struct {
			Text      string `json:"text"`
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return
		}
		execution.Logs.Stdout = append(execution.Logs.Stdout, event.Text)
		if opts.OnStdout != nil {
			opts.OnStdout(OutputMessage{Text: event.Text, Timestamp: event.Timestamp})
		}
	case "stderr":
		var event struct {
			Text      string `json:"text"`
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return
		}
		execution.Logs.Stderr = append(execution.Logs.Stderr, event.Text)
		if opts.OnStderr != nil {
			opts.OnStderr(OutputMessage{Text: event.Text, Timestamp: event.Timestamp, IsStderr: true})
		}
	case "error":
		var event struct {
			Name      string          `json:"name"`
			Value     string          `json:"value"`
			Traceback json.RawMessage `json:"traceback"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return
		}
		execErr := ExecutionError{
			Name:      event.Name,
			Value:     event.Value,
			Traceback: parseTraceback(event.Traceback),
		}
		if execErr.Traceback == nil {
			execErr.Traceback = []string{}
		}
		execution.Error = &execErr
		if opts.OnError != nil {
			opts.OnError(execErr)
		}
	case "number_of_executions":
		var event struct {
			ExecutionCount int `json:"execution_count"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return
		}
		execution.ExecutionCount = &event.ExecutionCount
	}
}

func parseTraceback(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	var lines []string
	if err := json.Unmarshal(raw, &lines); err == nil {
		return lines
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil
		}
		return []string{text}
	}

	return nil
}

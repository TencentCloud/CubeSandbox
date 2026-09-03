// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var baseURL = func() string {
	base := strings.TrimRight(os.Getenv("ENVD_BASE"), "/")
	if base == "" {
		base = "http://127.0.0.1:49984"
	}
	return base + "/process.Process/"
}()

var client = &http.Client{Timeout: 15 * time.Second}

type frameReader struct {
	body io.ReadCloser
	r    *bufio.Reader
}

func envelope(value any) []byte {
	payload, err := json.Marshal(value)
	must(err)
	frame := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

func request(method string, body io.Reader, contentLength int64, timeoutMS int) *http.Request {
	req, err := http.NewRequest(http.MethodPost, baseURL+method, body)
	must(err)
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.SetBasicAuth("root", "")
	req.ContentLength = contentLength
	if timeoutMS > 0 {
		req.Header.Set("Connect-Timeout-Ms", fmt.Sprint(timeoutMS))
	}
	return req
}

func openStream(method string, value any, timeoutMS int) *frameReader {
	body := envelope(value)
	resp, err := client.Do(request(method, bytes.NewReader(body), int64(len(body)), timeoutMS))
	must(err)
	if resp.StatusCode != http.StatusOK {
		panic(fmt.Sprintf("%s status %d", method, resp.StatusCode))
	}
	return &frameReader{body: resp.Body, r: bufio.NewReader(resp.Body)}
}

func (r *frameReader) frame() (byte, map[string]any) {
	header := make([]byte, 5)
	mustRead(r.r, header)
	size := binary.BigEndian.Uint32(header[1:])
	payload := make([]byte, size)
	mustRead(r.r, payload)
	var value map[string]any
	must(json.Unmarshal(payload, &value))
	return header[0], value
}

func unary(method string, value any) (int, map[string]any) {
	body, err := json.Marshal(value)
	must(err)
	req, err := http.NewRequest(http.MethodPost, baseURL+method, bytes.NewReader(body))
	must(err)
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("root", "")
	resp, err := client.Do(req)
	must(err)
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	must(err)
	result := map[string]any{}
	if len(payload) > 0 {
		must(json.Unmarshal(payload, &result))
	}
	return resp.StatusCode, result
}

func startPID(r *frameReader) int {
	flags, value := r.frame()
	assert(flags == 0, fmt.Sprintf("Start flags: %#x (%#v)", flags, value))
	event := value["event"].(map[string]any)
	start := event["start"].(map[string]any)
	return int(start["pid"].(float64))
}

func collect(r *frameReader) (map[string][]byte, map[string]any) {
	output := map[string][]byte{"stdout": {}, "stderr": {}, "pty": {}}
	var end map[string]any
	for {
		flags, value := r.frame()
		if event, ok := value["event"].(map[string]any); ok {
			if data, ok := event["data"].(map[string]any); ok {
				for _, name := range []string{"stdout", "stderr", "pty"} {
					if encoded, ok := data[name].(string); ok {
						decoded, err := base64.StdEncoding.DecodeString(encoded)
						must(err)
						output[name] = append(output[name], decoded...)
					}
				}
			}
			if terminal, ok := event["end"].(map[string]any); ok {
				end = terminal
			}
		}
		if flags&2 != 0 {
			assert(value["error"] == nil, fmt.Sprintf("EndStream error: %#v", value))
			break
		}
	}
	r.body.Close()
	return output, end
}

func pipeInput() {
	r := openStream("Start", map[string]any{
		"process": map[string]any{"cmd": "/bin/cat"},
		"stdin":   true,
		"tag":     "lifecycle-pipe",
	}, 0)
	pid := startPID(r)
	status, body := unary("SendInput", map[string]any{
		"process": map[string]any{"pid": pid},
		"input":   map[string]any{"stdin": base64.StdEncoding.EncodeToString([]byte("PIPE_OK\n"))},
	})
	assert(status == 200 && len(body) == 0, fmt.Sprintf("SendInput: %d %#v", status, body))
	status, body = unary("CloseStdin", map[string]any{"process": map[string]any{"pid": pid}})
	assert(status == 200 && len(body) == 0, fmt.Sprintf("CloseStdin: %d %#v", status, body))
	output, end := collect(r)
	assert(string(output["stdout"]) == "PIPE_OK\n", fmt.Sprintf("pipe output: %q", output["stdout"]))
	assert(end["status"] == "exit status 0", fmt.Sprintf("pipe end: %#v", end))
	fmt.Println("PASS pipe input/EOF")
}

func ptyInput() {
	r := openStream("Start", map[string]any{
		"process": map[string]any{
			"cmd":  "/bin/sh",
			"args": []string{"-c", "read x; printf 'PTY:%s\\n' \"$x\""},
		},
		"pty": map[string]any{"size": map[string]any{"cols": 80, "rows": 24}},
		"tag": "lifecycle-pty",
	}, 0)
	pid := startPID(r)
	status, body := unary("CloseStdin", map[string]any{"process": map[string]any{"pid": pid}})
	assert(status == 500 && strings.Contains(fmt.Sprint(body["message"]), "send Ctrl+D"), fmt.Sprintf("PTY close: %d %#v", status, body))
	status, body = unary("SendInput", map[string]any{
		"process": map[string]any{"pid": pid},
		"input":   map[string]any{"pty": base64.StdEncoding.EncodeToString([]byte("hello\n"))},
	})
	assert(status == 200 && len(body) == 0, fmt.Sprintf("PTY input: %d %#v", status, body))
	output, end := collect(r)
	assert(bytes.Contains(output["pty"], []byte("PTY:hello")), fmt.Sprintf("pty output: %q", output["pty"]))
	assert(end["status"] == "exit status 0", fmt.Sprintf("pty end: %#v", end))
	fmt.Println("PASS PTY input")
}

func streamInput() {
	process := openStream("Start", map[string]any{
		"process": map[string]any{"cmd": "/bin/cat"},
		"stdin":   true,
		"tag":     "lifecycle-stream",
	}, 0)
	pid := startPID(process)
	body := append(
		envelope(map[string]any{"start": map[string]any{"process": map[string]any{"pid": pid}}}),
		envelope(map[string]any{"data": map[string]any{"input": map[string]any{"stdin": base64.StdEncoding.EncodeToString([]byte("STREAM_OK\n"))}}})...,
	)
	reader, writer := io.Pipe()
	done := make(chan *http.Response, 1)
	go func() {
		resp, err := client.Do(request("StreamInput", reader, int64(len(body)), 0))
		must(err)
		done <- resp
	}()
	for _, part := range [][]byte{body[:3], body[3:9], body[9:]} {
		_, err := writer.Write(part)
		must(err)
		time.Sleep(10 * time.Millisecond)
	}
	must(writer.Close())
	resp := <-done
	assert(resp.StatusCode == 200, fmt.Sprintf("StreamInput status %d", resp.StatusCode))
	stream := &frameReader{body: resp.Body, r: bufio.NewReader(resp.Body)}
	flags, value := stream.frame()
	assert(flags == 0 && len(value) == 0, fmt.Sprintf("StreamInput message: %d %#v", flags, value))
	flags, value = stream.frame()
	assert(flags == 2 && len(value) == 0, fmt.Sprintf("StreamInput end: %d %#v", flags, value))
	stream.body.Close()
	status, result := unary("CloseStdin", map[string]any{"process": map[string]any{"pid": pid}})
	assert(status == 200 && len(result) == 0, fmt.Sprintf("stream close: %d %#v", status, result))
	output, end := collect(process)
	assert(string(output["stdout"]) == "STREAM_OK\n", fmt.Sprintf("stream output: %q", output["stdout"]))
	assert(end["status"] == "exit status 0", fmt.Sprintf("stream end: %#v", end))
	fmt.Println("PASS fragmented StreamInput")
}

func slowDeadline() {
	body := envelope(map[string]any{
		"process": map[string]any{
			"cmd":  "/bin/sh",
			"args": []string{"-c", "while :; do head -c 32768 /dev/zero; sleep 0.001; done"},
		},
		"stdin": false,
		"tag":   "lifecycle-slow",
	})
	resp, err := client.Do(request("Start", bytes.NewReader(body), int64(len(body)), 500))
	must(err)
	assert(resp.StatusCode == 200, fmt.Sprintf("slow Start status %d", resp.StatusCode))
	// Keep the response open and unread while the command deadline expires.
	time.Sleep(2 * time.Second)
	status, listing := unary("List", map[string]any{})
	assert(status == 200, fmt.Sprintf("List status %d", status))
	if processes, ok := listing["processes"].([]any); ok {
		for _, raw := range processes {
			process := raw.(map[string]any)
			assert(process["tag"] != "lifecycle-slow", fmt.Sprintf("slow process leaked: %#v", listing))
		}
	}
	resp.Body.Close()
	fmt.Println("PASS slow-client deadline cleanup")
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func mustRead(r io.Reader, data []byte) {
	_, err := io.ReadFull(r, data)
	must(err)
}

func assert(ok bool, message string) {
	if !ok {
		panic(message)
	}
}

func main() {
	pipeInput()
	ptyInput()
	streamInput()
	slowDeadline()
}

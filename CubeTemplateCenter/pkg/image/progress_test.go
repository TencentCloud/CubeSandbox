// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseHumanSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"512B", 512, true},
		{"5.0MiB", 5 * 1024 * 1024, true},
		{"30.0MiB", 30 * 1024 * 1024, true},
		{"1.5GiB", int64(1.5 * 1024 * 1024 * 1024), true},
		{"2 MB", 2 * 1000 * 1000, true},
		{"  10KiB ", 10 * 1024, true},
		{"abc", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseHumanSize(tc.in)
		if ok != tc.ok {
			t.Fatalf("parseHumanSize(%q) ok=%v want %v", tc.in, ok, tc.ok)
		}
		if ok && got != tc.want {
			t.Fatalf("parseHumanSize(%q)=%d want %d", tc.in, got, tc.want)
		}
	}
}

func TestSkopeoParserBytes(t *testing.T) {
	p := newSkopeoParser(0)
	if _, ok := p.feed("Getting image source signatures"); ok {
		t.Fatalf("non-progress line should not emit")
	}
	prog, ok := p.feed("Copying blob sha256:aaa [====>      ] 5.0MiB / 30.0MiB")
	if !ok {
		t.Fatalf("expected progress emit")
	}
	if prog.TotalBytes != 30*1024*1024 || prog.DownloadedBytes != 5*1024*1024 {
		t.Fatalf("unexpected progress %+v", prog)
	}
	if prog.TotalLayers != 1 {
		t.Fatalf("expected 1 layer, got %d", prog.TotalLayers)
	}
	// Second blob begins.
	prog, _ = p.feed("Copying blob sha256:bbb [=>         ] 1.0MiB / 10.0MiB")
	if prog.TotalLayers != 2 {
		t.Fatalf("expected 2 layers, got %d", prog.TotalLayers)
	}
	if prog.DownloadedBytes != 6*1024*1024 || prog.TotalBytes != 40*1024*1024 {
		t.Fatalf("unexpected cumulative progress %+v", prog)
	}
	// First blob completes.
	prog, _ = p.feed("Copying blob sha256:aaa done")
	if prog.CompletedLayers != 1 {
		t.Fatalf("expected 1 completed layer, got %d", prog.CompletedLayers)
	}
	if prog.DownloadedBytes != 30*1024*1024+1*1024*1024 {
		t.Fatalf("done should fill blob to total, got %d", prog.DownloadedBytes)
	}
}

func TestSkopeoParserTotalHint(t *testing.T) {
	p := newSkopeoParser(100 * 1024 * 1024)
	prog, _ := p.feed("Copying blob sha256:aaa [==>] 5.0MiB / 30.0MiB")
	if prog.TotalBytes != 100*1024*1024 {
		t.Fatalf("total hint should win while it is larger, got %d", prog.TotalBytes)
	}
	if prog.Percent <= 0 || prog.Percent >= 100 {
		t.Fatalf("percent out of range: %v", prog.Percent)
	}
}

func TestDockerParserLayers(t *testing.T) {
	p := newDockerParser()
	if _, ok := p.feed("latest: Pulling from library/ubuntu"); ok {
		t.Fatalf("header line should not emit")
	}
	p.feed("a1b2c3d4e5f6: Pulling fs layer")
	p.feed("f6e5d4c3b2a1: Pulling fs layer")
	prog, ok := p.feed("a1b2c3d4e5f6: Download complete")
	if !ok {
		t.Fatalf("expected emit")
	}
	if prog.TotalLayers != 2 {
		t.Fatalf("expected 2 layers, got %d", prog.TotalLayers)
	}
	if prog.CompletedLayers != 0 {
		t.Fatalf("download complete is not pull complete; got %d", prog.CompletedLayers)
	}
	prog, _ = p.feed("a1b2c3d4e5f6: Pull complete")
	if prog.CompletedLayers != 1 {
		t.Fatalf("expected 1 completed, got %d", prog.CompletedLayers)
	}
	prog, _ = p.feed("f6e5d4c3b2a1: Already exists")
	if prog.CompletedLayers != 2 {
		t.Fatalf("already exists counts as complete; got %d", prog.CompletedLayers)
	}
	if prog.Percent != 100 {
		t.Fatalf("expected 100%%, got %v", prog.Percent)
	}
}

func TestStreamCommandNilCallbackCapturesOutput(t *testing.T) {
	out, err := streamCommand(context.Background(), "", "sh", nil, nil, "-c", "printf 'hello\\nworld\\n'")
	if err != nil {
		t.Fatalf("streamCommand err: %v", err)
	}
	if got := string(out); got != "hello\nworld\n" {
		t.Fatalf("unexpected output %q", got)
	}
}

func TestStreamCommandParsesProgressAndError(t *testing.T) {
	var updates []PullProgress
	script := "printf 'Copying blob sha256:aaa [==>] 5.0MiB / 10.0MiB\\n'; " +
		"printf 'Copying blob sha256:aaa done\\n'; exit 3"
	out, err := streamCommand(context.Background(), "", "sh", newSkopeoParser(0),
		func(p PullProgress) { updates = append(updates, p) }, "-c", script)
	if err == nil {
		t.Fatalf("expected non-zero exit to produce error")
	}
	if len(updates) == 0 {
		t.Fatalf("expected progress updates")
	}
	last := updates[len(updates)-1]
	if last.CompletedLayers != 1 {
		t.Fatalf("expected final completed layer, got %+v", last)
	}
	if len(out) == 0 {
		t.Fatalf("expected captured output for diagnostics")
	}
}

func TestBoundedBufferKeepsTailWithinLimit(t *testing.T) {
	b := &boundedBuffer{max: 32}
	for i := 0; i < 100; i++ {
		b.add("0123456789")
	}
	if b.size > 32 {
		t.Fatalf("bounded buffer exceeded cap: size=%d", b.size)
	}
	if b.String() == "" {
		t.Fatalf("bounded buffer should retain the tail")
	}
}

func TestBoundedBufferTruncatesSingleOversizedLine(t *testing.T) {
	b := &boundedBuffer{max: 32}
	b.add(strings.Repeat("a", 128))
	if b.size > b.max {
		t.Fatalf("bounded buffer exceeded cap: size=%d max=%d", b.size, b.max)
	}
	if got := strings.TrimSpace(b.String()); len(got) != b.max-1 {
		t.Fatalf("retained line length=%d want %d", len(got), b.max-1)
	}
}

func TestStreamCommandDrainsOversizedLineWithoutHang(t *testing.T) {
	// A single line far exceeding the scanner token cap must not deadlock
	// cmd.Wait: the reader has to keep draining after Scan aborts. The
	// oversized line itself is intentionally dropped; what matters is that the
	// call completes instead of blocking forever on a full pipe.
	done := make(chan error, 1)
	go func() {
		_, err := streamCommand(context.Background(), "", "sh", nil, nil,
			"-c", "head -c 3000000 /dev/zero | tr '\\0' 'a'; echo; echo done")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("streamCommand errored: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("streamCommand hung on an oversized line")
	}
}

type blockingProgressParser struct {
	release chan struct{}
}

func (p *blockingProgressParser) feed(line string) (PullProgress, bool) {
	switch line {
	case "emit":
		return PullProgress{TotalLayers: 1}, true
	case "release":
		close(p.release)
	}
	return PullProgress{}, false
}

func TestStreamCommandProgressCallbackDoesNotHoldScanMutex(t *testing.T) {
	release := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := streamCommand(ctx, "", "sh", &blockingProgressParser{release: release}, func(PullProgress) {
		<-release
	}, "-c", "(printf 'emit\\n'; sleep 0.1; printf 'done\\n') & (sleep 0.05; printf 'release\\n' >&2) & wait")
	if err != nil {
		t.Fatalf("streamCommand should let stderr progress release a blocking callback: %v", err)
	}
}

func TestScanLinesCR(t *testing.T) {
	// carriage returns must split into discrete tokens.
	advance, token, err := scanLinesCR([]byte("abc\rdef"), false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if advance != 4 || string(token) != "abc" {
		t.Fatalf("advance=%d token=%q", advance, token)
	}
}

func TestScanLinesCRSkipsCRLFAsSingleSeparator(t *testing.T) {
	sc := bufio.NewScanner(strings.NewReader("abc\r\ndef\n"))
	sc.Split(scanLinesCR)

	var tokens []string
	for sc.Scan() {
		tokens = append(tokens, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got, want := strings.Join(tokens, ","), "abc,def"; got != want {
		t.Fatalf("tokens=%q want %q", got, want)
	}
}

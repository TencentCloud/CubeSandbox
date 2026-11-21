// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseXFSS3TrialOutputTakesTheResultLine(t *testing.T) {
	out := []byte("restoring...\nsome log\nVERIFY_RESULT {\"ok\":true,\"stage\":\"\",\"restore_ms\":812,\"agent_ms\":40,\"netns_interfaces\":[\"lo\"]}\n")
	r := parseXFSS3TrialOutput(out)
	if r == nil || !r.OK || r.RestoreMS != 812 || r.AgentMS != 40 || len(r.NetnsInterfaces) != 1 {
		t.Fatalf("parsed %+v", r)
	}
	if parseXFSS3TrialOutput([]byte("panic: boom\n")) != nil {
		t.Fatal("output without a result line must parse to nil")
	}
}

func TestClassifyTrialSeparatesCheckpointDefectsFromNodeLimits(t *testing.T) {
	for stage, want := range map[string]trialVerdict{
		"config":    trialDefect,
		"restore":   trialRetry,
		"agent":     trialRetry,
		"infra":     trialRetry,
		"isolation": trialRetry,
		"compat":    trialRetry,
		"deps":      trialRetry,
		"":          trialRetry,
	} {
		if got := classifyTrial(&XFSS3TrialResult{Stage: stage}); got != want {
			t.Errorf("stage %q: verdict %v, want %v", stage, got, want)
		}
	}
}

func TestXFSS3TrialBackoffGrowsAndCaps(t *testing.T) {
	if got := xfss3TrialBackoff(1); got != 30*time.Second {
		t.Fatalf("first retry after %s", got)
	}
	if got := xfss3TrialBackoff(3); got != 2*time.Minute {
		t.Fatalf("third retry after %s", got)
	}
	if got := xfss3TrialBackoff(50); got != 10*time.Minute {
		t.Fatalf("backoff not capped: %s", got)
	}
	if got := trialRetryPrefix("compat"); got != "retry(compat): " {
		t.Fatalf("prefix %q", got)
	}
}

func TestWritableDiskPaths(t *testing.T) {
	got, err := writableDiskPaths([]byte(`{"disks":[{"path":"/a/rootfs","readonly":false},{"path":"/b/ro","readonly":true}]}`))
	if err != nil || len(got) != 1 || got[0] != "/a/rootfs" {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := writableDiskPaths([]byte(`not json`)); err == nil {
		t.Fatal("bad config accepted")
	}
}

func TestRemoveXFSS3TrialRefusesUnsafeIDs(t *testing.T) {
	for _, id := range []string{"", "x", "ckpt-trial-../../etc", "ckpt-trial-a/b", "../ckpt-trial-a"} {
		if err := removeXFSS3Trial(id); err == nil {
			t.Errorf("removeXFSS3Trial(%q) accepted", id)
		}
	}
}

func TestProcessGoneMatchesTheTrialOnly(t *testing.T) {
	if !processGone(0, "ckpt-trial-x") {
		t.Fatal("pid 0 must count as gone")
	}
	self := os.Getpid()
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(self), "cmdline"))
	if err != nil {
		t.Skip("no /proc on this platform")
	}
	if !processGone(self, "ckpt-trial-not-in-my-cmdline") {
		t.Fatal("a live process that is not the trial must not block cleanup")
	}
	marker := strings.Split(string(cmdline), "\x00")[0]
	if processGone(self, marker) {
		t.Fatal("a live trial process must not count as gone")
	}
}

func TestPassedTrialCountsOnlyOnceCleanedUp(t *testing.T) {
	prev := removeXFSS3TrialFn
	t.Cleanup(func() { removeXFSS3TrialFn = prev })

	removeXFSS3TrialFn = func(string) error { return errors.New("device or resource busy") }
	res := &XFSS3TrialResult{OK: true, ProcessReleased: true}
	if err := finishPassedTrial(res, "ckpt-trial-x"); err == nil {
		t.Fatal("a trial whose files stay behind must not pass")
	}
	if err := xfss3TrialPassed(res); err == nil {
		t.Fatal("the ready gate must reject an uncleaned trial")
	}

	removeXFSS3TrialFn = func(string) error { return nil }
	res = &XFSS3TrialResult{OK: true, ProcessReleased: true}
	if err := finishPassedTrial(res, "ckpt-trial-x"); err != nil {
		t.Fatal(err)
	}
	if err := xfss3TrialPassed(res); err != nil {
		t.Fatalf("clean passed trial rejected: %v", err)
	}
	if err := xfss3TrialPassed(&XFSS3TrialResult{OK: true, CleanupCompleted: true}); err == nil {
		t.Fatal("the ready gate must reject a trial whose process was not confirmed gone")
	}
}

func TestLimitedBufferKeepsTailAndTrialResult(t *testing.T) {
	result := []byte("VERIFY_RESULT {\"ok\":true,\"restore_ms\":812}\n")
	for _, writes := range [][][]byte{
		{bytes.Repeat([]byte("log line\n"), xfss3TrialOutputLimit), result},
		{append(bytes.Repeat([]byte("log line\n"), xfss3TrialOutputLimit), result...)},
		{bytes.Repeat([]byte("x"), xfss3TrialOutputLimit), []byte("\n"), result[:15], result[15:]},
	} {
		b := &limitedBuffer{limit: xfss3TrialOutputLimit}
		var all []byte
		for _, p := range writes {
			all = append(all, p...)
			if n, err := b.Write(p); n != len(p) || err != nil {
				t.Fatalf("Write: %d %v", n, err)
			}
		}
		if got := b.Bytes(); !bytes.Equal(got, all[len(all)-b.limit:]) {
			t.Fatal("buffer did not retain exactly the last limit bytes")
		}
		if res := parseXFSS3TrialOutput(b.Bytes()); res == nil || !res.OK || res.RestoreMS != 812 {
			t.Fatalf("trial result lost after noisy output: %+v", res)
		}
	}
	b := &limitedBuffer{limit: 4}
	_, _ = b.Write([]byte("ab"))
	copyOfBytes := b.Bytes()
	copyOfBytes[0] = 'x'
	if string(b.Bytes()) != "ab" {
		t.Fatal("Bytes must return a copy")
	}
	_, _ = b.Write([]byte("cde"))
	if string(b.Bytes()) != "bcde" {
		t.Fatalf("tail=%q", b.Bytes())
	}
	_, _ = b.Write(nil)
	if string(b.Bytes()) != "bcde" {
		t.Fatal("empty write changed buffer")
	}
}

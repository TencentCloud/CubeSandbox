// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestDemoDirectDownload(t *testing.T) {
	for _, ttl := range []time.Duration{time.Hour, 2 * time.Hour} {
		t.Run(ttl.String(), func(t *testing.T) {
			var out bytes.Buffer
			if err := runDemo(ttl, &out); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"PASS source=template_center", "PASS source=s3", "expired_rejected=true", "[redacted]"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing demo assertion %q", want)
				}
			}
		})
	}
}

func TestDemoRejectsInvalidTTL(t *testing.T) {
	var out bytes.Buffer
	if err := runDemo(time.Second, &out); err == nil {
		t.Fatal("invalid TTL accepted")
	}
}

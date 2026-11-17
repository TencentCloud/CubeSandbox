// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package binding

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestStateSupplementaryCharactersRoundTrip(t *testing.T) {
	for _, prefix := range []string{
		"emoji/🙂/", "ideograph/𠮷/", "mixed/中文-é-🙂-𠮷/",
		"plane-one/\U00010000/", "last-plane/\U0010ffff/", `quoted/"<&>/`,
	} {
		t.Run(prefix, func(t *testing.T) {
			in := Binding{Bucket: "user-bucket", Prefix: prefix, CredRef: "user-a", Dynamic: true}
			raw, err := EncodeState(in)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range raw {
				if r > 0xffff {
					t.Fatalf("state contains raw supplementary character %U, incompatible with utf8mb3", r)
				}
			}
			got, err := ParseState(raw, "test-volume")
			if err != nil || got != in {
				t.Fatalf("round trip=%+v err=%v, want %+v", got, err, in)
			}
			// Older unescaped JSON states must remain readable as well.
			old, err := json.Marshal(stateJSON{Version: Version, Bucket: in.Bucket, Prefix: in.Prefix, CredRef: in.CredRef})
			if err != nil {
				t.Fatal(err)
			}
			got, err = ParseState(string(old), "test-volume")
			if err != nil || got != in {
				t.Fatalf("old state round trip=%+v err=%v", got, err)
			}
		})
	}
}

func TestStateEscapedByteLimit(t *testing.T) {
	const count = 75
	basePrefix := "boundary/" + strings.Repeat("🙂", count)
	encoded, err := json.Marshal(stateJSON{Version: Version, Bucket: "user-bucket", Prefix: basePrefix + "/", CredRef: "team-a"})
	if err != nil {
		t.Fatal(err)
	}
	// Each 4-byte rune becomes a 12-byte JSON surrogate escape.
	padding := MaxPrivateDataLen - (len(encoded) + count*8)
	if padding < 1 {
		t.Fatal("invalid boundary fixture")
	}
	for _, size := range []int{1023, 1024, 1025} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			prefix := basePrefix + strings.Repeat("a", padding+size-MaxPrivateDataLen) + "/"
			if _, err := NormalizePrefix(prefix); err != nil {
				t.Fatalf("fixture must meet the prefix limit: %v", err)
			}
			raw, err := EncodeState(Binding{Bucket: "user-bucket", Prefix: prefix, CredRef: "team-a", Dynamic: true})
			if size > MaxPrivateDataLen {
				if err == nil || !strings.Contains(err.Error(), "over the") {
					t.Fatalf("size=%d err=%v, want rejection of final escaped size", size, err)
				}
				return
			}
			if err != nil || len(raw) != size {
				t.Fatalf("encoded bytes=%d err=%v, want %d", len(raw), err, size)
			}
			got, err := ParseState(raw, "test-volume")
			if err != nil || got.Prefix != prefix {
				t.Fatalf("boundary round trip prefix=%q err=%v", got.Prefix, err)
			}
		})
	}
}

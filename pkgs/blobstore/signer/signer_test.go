// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package signer

import (
	"net/url"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	s := &Signer{Key: []byte("secret-key-32-bytes-minimum!!")}
	exp := time.Now().Add(5 * time.Minute)
	sig := s.SignGET("warehouse/blobs/a", exp)
	if err := s.VerifyGET("warehouse/blobs/a", sig, exp, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyGET("warehouse/blobs/b", sig, exp, time.Now()); err == nil {
		t.Fatal("cross-key replay must fail")
	}
	tampered := sig[:len(sig)-2] + "ff"
	if err := s.VerifyGET("warehouse/blobs/a", tampered, exp, time.Now()); err == nil {
		t.Fatal("tampered sig must fail")
	}
	if err := s.VerifyGET("warehouse/blobs/a", sig, exp, exp.Add(time.Second)); err == nil {
		t.Fatal("expired must fail")
	}
}

func TestURLRoundTrip(t *testing.T) {
	s := &Signer{Key: []byte("another-secret-key-32-bytes!!!")}
	raw, err := s.URL("http://10.0.0.11:3010", "/internal/warehouse/object", "warehouse/blobs/x", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	key, exp, sig, err := ParseGET(u)
	if err != nil {
		t.Fatal(err)
	}
	if key != "warehouse/blobs/x" {
		t.Fatalf("key %q", key)
	}
	if err := s.VerifyGET(key, sig, exp, time.Now()); err != nil {
		t.Fatal(err)
	}
	if s.Fingerprint() == "" || len(s.Fingerprint()) != 8 {
		t.Fatalf("fingerprint %q", s.Fingerprint())
	}
}

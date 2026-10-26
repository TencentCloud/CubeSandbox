// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package auth

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"
)

func BenchmarkSha256_New(b *testing.B) {
	key := []byte("1234567890")
	p := []byte("sdjfasjlkdfjlaejlksdjflksjdlijflsjeijrlsknlc,mansasdljflsdfsdfasljjlnlncvbksjhd")

	for i := 0; i <= b.N; i++ {
		h := hmac.New(sha256.New, key)
		h.Write(p)
		h.Sum(nil)
	}
}

func BenchmarkSha1_New(b *testing.B) {
	key := []byte("1234567890")
	p := []byte("sdjfasjlkdfjlaejlksdjflksjdlijflsjeijrlsknlc,mansasdljflsdfsdfasljjlnlncvbksjhd")

	for i := 0; i <= b.N; i++ {
		h := hmac.New(sha1.New, key)
		h.Write(p)
		h.Sum(nil)
	}
}

func BenchmarkAPISignWithNoBody(b *testing.B) {
	key := []byte("test")
	var body []byte

	p := &SignatureParams{
		Version:   DefaultVersion,
		UserID:    "user1",
		Timestamp: fmt.Sprintf("%d", time.Now().Unix()),
		Nonce:     "1111111111111",
		SgnMethod: "sha1",
		Signature: "",
	}

	err := GenSign(p, key, body)
	if err != nil {
		b.Fatal(err)
	}

	for i := 0; i < b.N; i++ {
		err = CheckSign(p, key, SignatureExpireTime)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func TestAPISign(t *testing.T) {
	key := []byte("test")
	var body []byte

	p := &SignatureParams{
		Version:   DefaultVersion,
		UserID:    "user1",
		Timestamp: fmt.Sprintf("%d", time.Now().Unix()),
		Nonce:     fmt.Sprintf("%d", time.Now().Unix()),
		SgnMethod: "sha1",
		Signature: "",
	}

	err := GenSign(p, key, body)
	if err != nil {
		t.Fatal(err)
	}

	t.Log(p.Signature)

	err = CheckSign(p, key, SignatureExpireTime)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(p.Signature)
}

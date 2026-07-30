// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package auth provides shared HMAC-SHA1 authentication header generation
// for CubeMaster API clients. Both the reporter and the cubemaster client
// use this package to ensure consistent auth header construction.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash"
	"net/http"
	"strconv"
	"time"
)

// Headers returns the set of HMAC authentication headers that CubeMaster's
// middleware expects. The signing algorithm mirrors CubeMaster's auth package
// exactly: sign("version.userID.timestamp.nonce.sgnMethod", secretKey).
func Headers(userID, secretKey string) (http.Header, error) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce, err := generateNonce()
	if err != nil {
		return nil, err
	}
	const version = "2023"
	const sgnMethod = "sha1"

	toSign := fmt.Sprintf("%s.%s.%s.%s.%s", version, userID, timestamp, nonce, sgnMethod)

	sig, err := hmacSign(sgnMethod, []byte(secretKey), []byte(toSign))
	if err != nil {
		return nil, err
	}

	h := http.Header{}
	h.Set("cube_version", version)
	h.Set("cube_user_id", userID)
	h.Set("cube_timestamp", timestamp)
	h.Set("cube_nonce", nonce)
	h.Set("cube_sgn_method", sgnMethod)
	h.Set("cube_signature", sig)
	return h, nil
}

func generateNonce() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	nonce := binary.BigEndian.Uint64(buf[:]) & (1<<63 - 1)
	return strconv.FormatUint(nonce, 10), nil
}

func hmacSign(method string, key, data []byte) (string, error) {
	var h hash.Hash
	if method == "sha256" {
		h = hmac.New(sha256.New, key)
	} else {
		h = hmac.New(sha1.New, key)
	}
	if _, err := h.Write(data); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

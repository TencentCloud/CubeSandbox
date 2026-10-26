// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package auth provides a simple interface for signing and verifying
package auth

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"hash"
	"math"
	"math/rand"
	"strconv"
	"time"
)

type SignatureParams struct {
	Version   string `json:"cube_version"`
	UserID    string `json:"cube_user_id"`
	Timestamp string `json:"cube_timestamp"`
	Nonce     string `json:"cube_nonce"`
	SgnMethod string `json:"cube_sgn_method"`

	Signature string `json:"cube_signature"`
}

func DefaultNew(appID, userID, hmacWay string) *SignatureParams {
	_ = appID
	return &SignatureParams{
		Version:   "2023",
		UserID:    userID,
		Timestamp: strconv.FormatInt(time.Now().Unix(), 10),
		Nonce:     strconv.FormatInt(rand.Int63n(math.MaxInt64), 10),
		SgnMethod: hmacWay,
	}
}

func (s *SignatureParams) toBeSignedString() []byte {
	buf := make([]byte, 0, 256)
	buf = append(buf, s.Version...)
	buf = append(buf, '.')
	buf = append(buf, s.UserID...)
	buf = append(buf, '.')
	buf = append(buf, s.Timestamp...)
	buf = append(buf, '.')
	buf = append(buf, s.Nonce...)
	buf = append(buf, '.')
	buf = append(buf, s.SgnMethod...)

	return buf
}

func (s *SignatureParams) SignedString(key, body []byte) error {

	tobss := s.toBeSignedString()
	sgn, err := SignedString(s.SgnMethod, key, tobss)
	if err != nil {
		return err
	}
	s.Signature = sgn
	return nil
}

func SignedString(hmacWay string, key, p []byte) (string, error) {
	var h hash.Hash
	if hmacWay == "sha256" {
		h = hmac.New(sha256.New, key)
	} else {
		h = hmac.New(sha1.New, key)
	}
	_, err := h.Write(p)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}

func (s *SignatureParams) check(gen bool) error {
	if s == nil {
		return errors.New("signature params is nil")
	}
	if s.UserID == "" {
		return errors.New("signature user id is empty")
	}
	if s.Timestamp == "" {
		return errors.New("signature timestamp is empty")
	}
	if s.Nonce == "" {
		return errors.New("signature nonce is empty")
	}
	if s.SgnMethod == "" {
		return errors.New("signature method is empty")
	}

	if s.SgnMethod != "sha1" && s.SgnMethod != "sha256" {
		return errors.New("signature method is invalid")
	}

	if !gen && s.Signature == "" {
		return errors.New("signature is empty")
	}
	return nil
}

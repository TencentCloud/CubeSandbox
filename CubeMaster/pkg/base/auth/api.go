// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package auth provides auth related functions
package auth

import (
	"errors"
	"strconv"
	"time"
)

const (
	DefaultVersion            = "2023"
	SHA256                    = "sha256"
	SHA1                      = "sha1"
	SignatureExpireTime int64 = 2 * 60
)

var (
	ErrVersion          = errors.New("signature version failed")
	ErrSignExpire       = errors.New("signature expire")
	ErrSignVerifyFailed = errors.New("signature verify failed")
)

func CheckSign(p *SignatureParams, secret_key []byte, expireTime int64) error {
	err := p.check(false)
	if err != nil {
		return err
	}

	if p.Version != DefaultVersion {
		return ErrVersion
	}

	timestamp, err := strconv.ParseInt(p.Timestamp, 10, 64)
	if err != nil {
		return err
	}

	if expireTime == 0 {
		expireTime = SignatureExpireTime
	}

	curTime := time.Now().Unix()
	if curTime-timestamp > expireTime ||
		timestamp-curTime > expireTime {
		return ErrSignExpire
	}

	oldSign := p.Signature
	p.Signature = ""
	err = p.SignedString([]byte(secret_key), nil)
	if err != nil {
		return err
	}

	if oldSign != p.Signature {
		return ErrSignVerifyFailed
	}
	return nil
}

func GenSign(p *SignatureParams, secret_key, body []byte) error {
	err := p.check(true)
	if err != nil {
		return err
	}
	err = p.SignedString([]byte(secret_key), body)
	if err != nil {
		return err
	}
	return nil
}

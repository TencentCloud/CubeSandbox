// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package signer mints and verifies HMAC-SHA256 GET URLs for the fs backend.
package signer

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// Version is included in the signed payload so the algorithm can change
	// without colliding with old URLs.
	Version  = "blobstore-v1"
	QueryKey = "key"
	QueryExp = "exp"
	QuerySig = "sig"
)

var (
	ErrExpired   = errors.New("blobstore/signer: url expired")
	ErrBadSig    = errors.New("blobstore/signer: bad signature")
	ErrBadParams = errors.New("blobstore/signer: missing or invalid params")
)

// Signer issues and checks HMAC-SHA256 GET signatures.
type Signer struct {
	Key []byte
}

// Fingerprint returns the first 8 hex chars of SHA-256(key) so replicas can
// compare signing material in logs without leaking the key.
func (s *Signer) Fingerprint() string {
	if s == nil || len(s.Key) == 0 {
		return ""
	}
	sum := sha256.Sum256(s.Key)
	return hex.EncodeToString(sum[:])[:8]
}

func payload(method, objectKey string, exp int64) string {
	return Version + "\n" + method + "\n" + objectKey + "\n" + strconv.FormatInt(exp, 10)
}

// SignGET returns the hex HMAC for a GET of objectKey that expires at exp.
func (s *Signer) SignGET(objectKey string, exp time.Time) string {
	mac := hmac.New(sha256.New, s.Key)
	_, _ = mac.Write([]byte(payload("GET", objectKey, exp.Unix())))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyGET checks sig for objectKey. now is the verification clock.
func (s *Signer) VerifyGET(objectKey, sig string, exp, now time.Time) error {
	if s == nil || len(s.Key) == 0 || objectKey == "" || sig == "" {
		return ErrBadParams
	}
	if now.After(exp) {
		return ErrExpired
	}
	want := s.SignGET(objectKey, exp)
	got, err := hex.DecodeString(sig)
	if err != nil {
		return ErrBadSig
	}
	wantB, err := hex.DecodeString(want)
	if err != nil {
		return ErrBadSig
	}
	if !hmac.Equal(got, wantB) {
		return ErrBadSig
	}
	return nil
}

// URL builds `{base}{mount}?key=&exp=&sig=`. base must already be the
// node-reachable origin (scheme + host[:port]); mount is the path prefix
// such as "/internal/warehouse/object".
func (s *Signer) URL(base, mount, objectKey string, ttl time.Duration) (string, error) {
	if s == nil || len(s.Key) == 0 {
		return "", fmt.Errorf("blobstore/signer: empty signing key")
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	exp := time.Now().Add(ttl)
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil {
		return "", fmt.Errorf("blobstore/signer: parse base: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("blobstore/signer: base %q is not an absolute URL", base)
	}
	mount = strings.TrimSpace(mount)
	if mount == "" {
		mount = "/"
	}
	if !strings.HasPrefix(mount, "/") {
		mount = "/" + mount
	}
	u.Path = strings.TrimRight(u.Path, "/") + mount
	q := u.Query()
	q.Set(QueryKey, objectKey)
	q.Set(QueryExp, strconv.FormatInt(exp.Unix(), 10))
	q.Set(QuerySig, s.SignGET(objectKey, exp))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ParseGET extracts key/exp/sig from a request URL.
func ParseGET(u *url.URL) (objectKey string, exp time.Time, sig string, err error) {
	if u == nil {
		return "", time.Time{}, "", ErrBadParams
	}
	q := u.Query()
	objectKey = strings.TrimSpace(q.Get(QueryKey))
	sig = strings.TrimSpace(q.Get(QuerySig))
	rawExp := strings.TrimSpace(q.Get(QueryExp))
	if objectKey == "" || sig == "" || rawExp == "" {
		return "", time.Time{}, "", ErrBadParams
	}
	n, err := strconv.ParseInt(rawExp, 10, 64)
	if err != nil {
		return "", time.Time{}, "", ErrBadParams
	}
	return objectKey, time.Unix(n, 0).UTC(), sig, nil
}

// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"net/http"
)

var (
	errBucketAbsent       = errors.New("S3 confirmed bucket absent")
	errUnverifiedNotFound = errors.New("HTTP 404 without confirmed S3 bucket absence")
	errUnverified503      = errors.New("HTTP 503 without confirmed S3 throttling")
	errSlowDown           = errors.New("S3 returned SlowDown")
)

// Inspect the wire response before minio-go can infer an S3 error code from
// an HTTP status. This transport is used with SDK retries disabled.
type destroyTransport struct {
	http.RoundTripper
	bucket string
}

func (t destroyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusServiceUnavailable {
		return resp, nil
	}
	defer resp.Body.Close()

	var body struct {
		XMLName    xml.Name `xml:"Error"`
		Code       string
		BucketName string
	}
	const maxErrorBytes = 16 * 1024
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes+1))
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	explicit := (media == "application/xml" || media == "text/xml") &&
		readErr == nil && len(data) <= maxErrorBytes && xml.Unmarshal(data, &body) == nil &&
		(body.BucketName == "" || body.BucketName == t.bucket)
	if resp.StatusCode == http.StatusNotFound {
		if explicit && body.Code == "NoSuchBucket" {
			return nil, errBucketAbsent
		}
		return nil, errUnverifiedNotFound
	}
	if explicit && body.Code == "SlowDown" {
		return nil, errSlowDown
	}
	return nil, errUnverified503
}

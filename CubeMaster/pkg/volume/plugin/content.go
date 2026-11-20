// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"context"
	"time"
)

const (
	// DirectAccessProviderS3 identifies an S3-compatible object-storage endpoint.
	DirectAccessProviderS3 = "s3"
	// DirectAccessProviderCOS is a legacy provider value retained for existing hooks.
	DirectAccessProviderCOS = "cos"
)

// DirectAccessCredentials are short-lived, volume-scoped credentials returned
// by a storage driver. They are sent to the SDK and must never be logged.
type DirectAccessCredentials struct {
	// Provider is the storage API family, not the vendor name.
	Provider        string
	Endpoint        string
	Bucket          string
	Region          string
	Prefix          string
	AddressingStyle string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	ExpiresAt       time.Time
}

// ContentPlugin is a control-plane hook that vends short-lived credentials for
// direct SDK-to-storage access. It does not proxy file reads or writes.
type ContentPlugin interface {
	Name() string
	IssueCredentials(ctx context.Context, volumeID, privateData, subject string) (*DirectAccessCredentials, error)
}

var contentRegistry = map[string]ContentPlugin{}

// RegisterContent adds the credential hook under the same driver name used by
// ControllerPlugin and VolumeRecord.Driver. Duplicate names panic at startup.
func RegisterContent(p ContentPlugin) {
	if _, dup := contentRegistry[p.Name()]; dup {
		panic("volume content plugin already registered: " + p.Name())
	}
	contentRegistry[p.Name()] = p
}

// GetContent returns the registered direct-access credential hook for a driver.
func GetContent(name string) (ContentPlugin, bool) {
	p, ok := contentRegistry[name]
	return p, ok
}

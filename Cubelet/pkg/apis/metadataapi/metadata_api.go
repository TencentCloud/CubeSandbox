// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package metadataapi

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

type MetadataAPI interface {
	GetInstanceID(ctx context.Context) (string, error)
	GetLocalIpv4(ctx context.Context) (string, error)
}

var (
	Client MetadataAPI = &cachedMetaclient{}
)

type cachedMetaclient struct {
	instanceID string
	localIPv4  string
}

func (c *cachedMetaclient) GetInstanceID(ctx context.Context) (string, error) {
	_ = ctx
	if c.instanceID == "" {
		instanceID, err := utils.GetInstanceID()
		if err != nil {
			return "", err
		}
		c.instanceID = instanceID
	}
	return c.instanceID, nil
}

func (c *cachedMetaclient) GetLocalIpv4(ctx context.Context) (string, error) {
	_ = ctx
	if c.localIPv4 == "" {
		localIPv4, err := utils.GetLocalIpv4()
		if err != nil {
			return "", err
		}
		c.localIPv4 = localIPv4
	}
	return c.localIPv4, nil
}

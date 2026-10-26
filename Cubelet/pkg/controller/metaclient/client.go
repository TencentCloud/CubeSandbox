// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package metaclient

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/masterclient"
	"k8s.io/client-go/rest"
)

type ClientCache struct {
	MasterClient *masterclient.Client
}

var (
	mu            sync.Mutex
	cachedClients *ClientCache
)

func InitConfig(cfg *rest.Config, _ time.Duration) error {
	if cfg == nil || cfg.Host == "" {
		return fmt.Errorf("metadata config host is empty")
	}
	mu.Lock()
	defer mu.Unlock()
	cachedClients = &ClientCache{
		MasterClient: masterclient.New(cfg.Host, cfg.Timeout),
	}
	return nil
}

func GetCubeClient() (*ClientCache, error) {
	mu.Lock()
	defer mu.Unlock()
	if cachedClients == nil || cachedClients.MasterClient == nil {
		return nil, fmt.Errorf("metadata client has not been initialized")
	}
	return cachedClients, nil
}

func (cc *ClientCache) StartInformers(stopCh <-chan struct{}) {
	_ = stopCh
}

func (cc *ClientCache) WaitForCacheSync(stopCh <-chan struct{}) {
	_ = stopCh
}

func (cc *ClientCache) WaitForHealthly(stopCh <-chan struct{}) bool {
	if cc == nil || cc.MasterClient == nil {
		return false
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := cc.MasterClient.Readyz(context.Background()); err == nil {
			return true
		}
		select {
		case <-stopCh:
			return false
		case <-ticker.C:
		}
	}
}

func (cc *ClientCache) GetCubeClientSet() interface{} {
	return nil
}

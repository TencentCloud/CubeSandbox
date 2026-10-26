// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package blobstore

import (
	"context"
	"fmt"
	"strings"
)

// Driver is what an engine-specific package must provide.
type Driver interface {
	Name() string
	Open(ctx context.Context, cfg Config) (Store, error)
}

var driverRegistry = map[string]Driver{}

// Register makes a driver available by name. Re-registering the same name
// panics; this is a programmer error caught at process start.
func Register(d Driver) {
	if d == nil {
		panic("blobstore: Register(nil) driver")
	}
	name := d.Name()
	if name == "" {
		panic("blobstore: driver returned empty Name")
	}
	if _, exists := driverRegistry[name]; exists {
		panic(fmt.Sprintf("blobstore: driver %q already registered", name))
	}
	driverRegistry[name] = d
}

func resolveDriver(name string) (Driver, error) {
	if name == "" {
		return nil, fmt.Errorf("blobstore: driver name is empty")
	}
	d, ok := driverRegistry[name]
	if !ok {
		return nil, fmt.Errorf("blobstore: driver %q is not registered (did you forget a blank import?)", name)
	}
	return d, nil
}

// Open constructs a Store for cfg.Driver. The caller must blank-import the
// matching driver package so it can register itself.
func Open(ctx context.Context, cfg Config) (Store, error) {
	cfg.Driver = strings.TrimSpace(cfg.Driver)
	drv, err := resolveDriver(cfg.Driver)
	if err != nil {
		return nil, err
	}
	if cfg.Prefix != "" {
		if err := ValidatePrefix(cfg.Prefix); err != nil {
			return nil, fmt.Errorf("blobstore: prefix: %w", err)
		}
	}
	return drv.Open(ctx, cfg)
}

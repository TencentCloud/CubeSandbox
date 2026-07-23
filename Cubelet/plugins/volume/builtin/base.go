// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package builtin provides the base type for built-in VolumePlugin
// implementations.  A built-in plugin is compiled directly into cubelet and
// registered at startup with volume.Register().
//
// Embedding Base gives a plugin sensible no-op defaults for Init and Close so
// concrete types only need to implement Name, Attach, and Detach.
package builtin

import (
	"context"

	"github.com/tencentcloud/CubeSandbox/Cubelet/plugins/volume"
)

// Base is an embeddable struct that provides default implementations of the
// less interesting VolumePlugin methods.  Concrete built-in plugins embed it
// and override what they need.
type Base struct{}

func (Base) PluginType() volume.PluginType                       { return volume.PluginTypeBuiltin }
func (Base) Init(_ context.Context, _ volume.PluginConfig) error { return nil }
func (Base) Close() error                                        { return nil }

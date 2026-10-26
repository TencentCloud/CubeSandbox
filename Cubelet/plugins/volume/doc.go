// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Package volume defines the VolumePlugin interface and the three plugin
// loading mechanisms (built-in, binary, RPC) used for the plugin_volume
// VolumeSource type introduced in cubebox.proto.
//
// SCOPE: This package handles ONLY volumes whose VolumeSource has a non-nil
// plugin_volume field.  The existing empty_dir / host_dir_volumes / image /
// sandbox_path sources are handled entirely by the existing storage pipeline
// and are NOT affected by this package in any way.
package volume

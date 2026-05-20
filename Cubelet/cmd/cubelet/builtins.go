// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package main

import (
	_ "github.com/containerd/containerd/v2/core/metrics/cgroups"
	_ "github.com/containerd/containerd/v2/core/metrics/cgroups/v2"
	_ "github.com/containerd/containerd/v2/core/runtime/v2"
	_ "github.com/containerd/containerd/v2/plugins/content/local/plugin"
	_ "github.com/containerd/containerd/v2/plugins/diff/walking/plugin"
	_ "github.com/containerd/containerd/v2/plugins/events"
	_ "github.com/containerd/containerd/v2/plugins/gc"
	_ "github.com/containerd/containerd/v2/plugins/imageverifier"
	_ "github.com/containerd/containerd/v2/plugins/leases"

	_ "github.com/containerd/containerd/v2/plugins/mount/erofs"
	_ "github.com/containerd/containerd/v2/plugins/restart"
	_ "github.com/containerd/containerd/v2/plugins/services/containers"
	_ "github.com/containerd/containerd/v2/plugins/services/content"
	_ "github.com/containerd/containerd/v2/plugins/services/diff"
	_ "github.com/containerd/containerd/v2/plugins/services/events"
	_ "github.com/containerd/containerd/v2/plugins/services/healthcheck"
	_ "github.com/containerd/containerd/v2/plugins/services/images"
	_ "github.com/containerd/containerd/v2/plugins/services/introspection"
	_ "github.com/containerd/containerd/v2/plugins/services/leases"
	_ "github.com/containerd/containerd/v2/plugins/services/namespaces"
	_ "github.com/containerd/containerd/v2/plugins/services/opt"
	_ "github.com/containerd/containerd/v2/plugins/services/sandbox"
	_ "github.com/containerd/containerd/v2/plugins/services/snapshots"
	_ "github.com/containerd/containerd/v2/plugins/services/streaming"
	_ "github.com/containerd/containerd/v2/plugins/services/tasks"
	_ "github.com/containerd/containerd/v2/plugins/services/transfer"
	_ "github.com/containerd/containerd/v2/plugins/services/version"
	_ "github.com/containerd/containerd/v2/plugins/snapshots/native/plugin"
	_ "github.com/containerd/containerd/v2/plugins/streaming"
	_ "github.com/containerd/containerd/v2/plugins/transfer"

	_ "github.com/tencentcloud/CubeSandbox/Cubelet/network"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/backup"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cbri"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cbri/cubeboxcbri"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/chi/plugin"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/controller"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/images"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/appsnapshot"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cgroup"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/createid"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/cubes"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/metric"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/netfile"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/multimeta"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/runtime"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/snapshots/overlay/plugin"

	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/internals/sandbox"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/cube/multimeta"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/metadata"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/mount"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/mount/cbind"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/plugins/workflow/plugin"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/services/cubebox"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/services/gc"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/services/images"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/services/nbi"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/services/version"
	_ "github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package sandboxlog holds the host path where the shim writes init logs.
//
// Regular (non-template) sandboxes:
//
//	/data/cubelet/log/<sandbox-id>/stdout
//	/data/cubelet/log/<sandbox-id>/stderr
//
// The shim creates and deletes these files. cubecli reads them on the host
// filesystem (no mount-namespace entry). Template-build logs stay under
// /data/log/template.
package sandboxlog

// Dir is the host directory for sandbox init logs. Not configurable.
const Dir = "/data/cubelet/log"

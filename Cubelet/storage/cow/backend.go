// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cow

import (
	"fmt"
	"strings"
)

// Backend type values carried on Master → Cubelet requests (create / pause /
// snapshot / resume). Empty means the historical default (XFS / xfscow).
const (
	BackendXFS = "xfs"
	BackendS3  = "s3"
)

// NormalizeBackend maps request/config aliases onto BackendXFS or BackendS3.
// Empty input defaults to BackendXFS for backward compatibility.
func NormalizeBackend(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", BackendXFS, NameXfsCow, "cow", "cubecow", "reflink":
		return BackendXFS, nil
	case BackendS3: // same literal as NameS3 ("s3")
		return BackendS3, nil
	default:
		return "", fmt.Errorf("unsupported cow backend %q (want %q or %q)", raw, BackendXFS, BackendS3)
	}
}

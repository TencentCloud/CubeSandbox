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
//
// BackendJFS is the XFS data plane moved onto a JuiceFS mount: the same
// reflink Store, layout and package flow, with files cloned by
// copy_file_range (a metadata-only clone on JuiceFS) instead of FICLONE.
// Memory is served by the page cache and the JuiceFS block cache.
const (
	BackendXFS = "xfs"
	BackendS3  = "s3"
	BackendJFS = "jfs"
)

// NormalizeBackend maps request/config aliases onto BackendXFS, BackendS3 or
// BackendJFS. Empty input defaults to BackendXFS for backward compatibility.
func NormalizeBackend(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", BackendXFS, NameXfsCow, "cow", "cubecow", "reflink":
		return BackendXFS, nil
	case BackendS3: // same literal as NameS3 ("s3")
		return BackendS3, nil
	case BackendJFS, "juicefs":
		return BackendJFS, nil
	default:
		return "", fmt.Errorf("unsupported cow backend %q (want %q, %q or %q)", raw, BackendXFS, BackendS3, BackendJFS)
	}
}

// IsReflinkBackend reports whether backend runs on the file-clone Store
// (xfs or jfs), as opposed to S3 lvols.
func IsReflinkBackend(backend string) bool {
	normalized, err := NormalizeBackend(backend)
	return err == nil && (normalized == BackendXFS || normalized == BackendJFS)
}

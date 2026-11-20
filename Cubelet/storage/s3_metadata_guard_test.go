// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage/cow"
)

// A pause keeps writing its package into the metadata disk mounted at the
// package's metadata directory. If that mount was taken away underneath it,
// the writes land on the parent file system and the exported disk lacks
// them, so the check must fail rather than let the package be sealed.
func TestVerifyS3MetadataMounted(t *testing.T) {
	stubS3MetadataMounts(t)
	const dir = "/data/cubelet/storage/s3/pause-snapshots/snap-1/metadata"

	require.NoError(t, mountS3MetadataDevice("/dev/fake-meta", dir))
	require.NoError(t, VerifyS3MetadataMounted(cow.BackendS3, dir))
	require.NoError(t, VerifyS3MetadataMounted(cow.BackendS3, dir+"/"), "path is compared cleaned")

	require.NoError(t, unmountS3MetadataPath(dir))
	err := VerifyS3MetadataMounted(cow.BackendS3, dir)
	require.ErrorIs(t, err, ErrS3MetadataMountLost)
	require.Contains(t, err.Error(), dir)
}

func TestVerifyS3MetadataMountedIgnoresOtherBackends(t *testing.T) {
	stubS3MetadataMounts(t)
	require.NoError(t, VerifyS3MetadataMounted(cow.BackendXFS, "/never/mounted"))
	require.NoError(t, VerifyS3MetadataMounted(cow.BackendJFS, "/never/mounted"))
}

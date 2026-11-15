// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package storage

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func replaceS3MetadataMountExec(t *testing.T, replacement func([]string, time.Duration) (string, string, error)) {
	t.Helper()
	original := execS3MetadataMount
	execS3MetadataMount = replacement
	t.Cleanup(func() { execS3MetadataMount = original })
}

func TestMountS3MetadataDeviceKeepsExecErrorAndStderr(t *testing.T) {
	sentinel := errors.New("exit status 32")
	replaceS3MetadataMountExec(t, func(argv []string, timeout time.Duration) (string, string, error) {
		require.Equal(t, []string{"mount", "-t", "ext4", "/dev/nvme7n1", "/meta"}, argv,
			"metadata disks are always ext4; mount must not probe for the type")
		require.Equal(t, cmdTimeout, timeout)
		return "", "mount: wrong fs type\n", sentinel
	})

	err := mountS3MetadataDeviceImpl("/dev/nvme7n1", "/meta")
	require.ErrorIs(t, err, sentinel)
	require.Contains(t, err.Error(), "/dev/nvme7n1")
	require.Contains(t, err.Error(), "mount: wrong fs type")
	require.Contains(t, err.Error(), "timeout "+cmdTimeout.String())
}

func TestMountS3MetadataDeviceExplainsAKilledMount(t *testing.T) {
	// A mount killed at its deadline leaves stderr empty; the error must
	// still say what happened and how long it ran.
	sentinel := errors.New("signal: killed")
	replaceS3MetadataMountExec(t, func([]string, time.Duration) (string, string, error) {
		time.Sleep(20 * time.Millisecond)
		return "", "", sentinel
	})

	err := mountS3MetadataDeviceImpl("/dev/nvme8n1", "/meta")
	require.ErrorIs(t, err, sentinel)
	require.Contains(t, err.Error(), "signal: killed")
	require.Regexp(t, `failed after [0-9]+ms`, err.Error())
}

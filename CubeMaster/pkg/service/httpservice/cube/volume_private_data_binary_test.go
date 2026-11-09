// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/plugin"
	binaryplugin "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/volume/plugin/binary"
)

// This fixture uses only the HTTP and binary protocols, so it can also run
// against the previous ControllerPlugin interface to reproduce the missing
// private_data forwarding before changing production code.
func TestVolumeHTTP_PrivateDataBinaryLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("binary fixture requires a POSIX shell")
	}
	r, db, _ := newVolumeTestEngine(t)
	script := filepath.Join(t.TempDir(), "volume-private-data.sh")
	const content = `#!/bin/sh
set -eu
op=
private_data=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --op) op="$2"; shift 2 ;;
    --volume-id|--name) shift 2 ;;
    --private-data) private_data="$2"; shift 2 ;;
    *) echo 'unexpected argument' >&2; exit 1 ;;
  esac
done
case "$op" in
  create)
    if [ "$private_data" != 'issue-2086-input-A' ]; then
      echo 'create did not receive private_data' >&2
      exit 1
    fi
    printf '%s\n' '{"private_data":"issue-2086-state-B","error":""}'
    ;;
  destroy)
    if [ "$private_data" != 'issue-2086-state-B' ]; then
      echo 'destroy did not receive persisted state' >&2
      exit 1
    fi
    printf '%s\n' '{"error":""}'
    ;;
  *) echo 'unexpected operation' >&2; exit 1 ;;
esac
`
	require.NoError(t, os.WriteFile(script, []byte(content), 0o755))
	p := binaryplugin.New("private-data-binary", script)
	plugin.Register(p)
	t.Cleanup(func() { plugin.UnregisterForTest(p.Name()) })

	_, created := doVolumeRequest(t, r, http.MethodPost, "/cube/volume",
		fmt.Sprintf(`{"name":"vol-private-data-binary","driver":%q,"private_data":"issue-2086-input-A"}`, p.Name()))
	require.Equal(t, 0, created.Ret.RetCode, "create result: %s", created.Ret.RetMsg)
	var record models.VolumeRecord
	require.NoError(t, db.Where("volume_id = ?", "vol-private-data-binary").First(&record).Error)
	assert.Equal(t, "issue-2086-state-B", record.PrivateData)

	_, deleted := doVolumeRequest(t, r, http.MethodDelete, "/cube/volume/vol-private-data-binary", "")
	require.Equal(t, 0, deleted.Ret.RetCode, "destroy result: %s", deleted.Ret.RetMsg)
	var count int64
	require.NoError(t, db.Model(&models.VolumeRecord{}).Count(&count).Error)
	assert.Zero(t, count)
}

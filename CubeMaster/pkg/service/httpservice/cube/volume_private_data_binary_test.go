// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cube

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
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

func TestVolumeHTTP_BinaryCallerErrorDoesNotExposeStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("binary fixture requires a POSIX shell")
	}
	r, db, _ := newVolumeTestEngine(t)
	script := filepath.Join(t.TempDir(), "caller-error.sh")
	const scriptContent = `#!/bin/sh
printf '%s\n' 'credential-file=/private/credentials/secret.conf' >&2
printf '%s\n' '{"error":"dynamic private_data requires cred_ref","error_kind":"invalid_argument","private_data":"must-not-be-persisted"}'
exit 1
`
	require.NoError(t, os.WriteFile(script, []byte(scriptContent), 0o700))
	p := binaryplugin.New("typed-error-binary", script)
	plugin.Register(p)
	t.Cleanup(func() { plugin.UnregisterForTest(p.Name()) })
	w, env := doVolumeRequest(t, r, http.MethodPost, "/cube/volume",
		volumeCreateBody(t, "vol-binary-caller-error", p.Name(), `{"bucket":"test-bucket"}`))
	assert.Equal(t, int(errorcode.ErrorCode_MasterParamsError), env.Ret.RetCode)
	assert.Contains(t, env.Ret.RetMsg, "dynamic private_data requires cred_ref")
	for _, secret := range []string{"credential-file", "/private/credentials", "secret.conf", "must-not-be-persisted", script, "exit status"} {
		assert.NotContains(t, w.Body.String(), secret)
	}
	assert.Nil(t, env.Volume)
	var count int64
	require.NoError(t, db.Model(&models.VolumeRecord{}).Count(&count).Error)
	assert.Zero(t, count)
}

// A prebuilt S3 binary keeps the ordinary unit suite independent of another
// module's compiler invocation. Acceptance runs explicitly set this path.
func TestVolumeHTTP_S3BinaryErrorKinds(t *testing.T) {
	binaryPath := os.Getenv("CUBE_TEST_S3_BINARY")
	if binaryPath == "" {
		t.Skip("set CUBE_TEST_S3_BINARY to a prebuilt cube-volume-s3 for cross-process integration")
	}
	require.True(t, filepath.IsAbs(binaryPath), "CUBE_TEST_S3_BINARY must be absolute")
	info, err := os.Stat(binaryPath)
	require.NoError(t, err, "explicit S3 binary path must exist")
	require.False(t, info.IsDir(), "S3 binary path must be a file")
	for _, tc := range []struct {
		name         string
		input        string
		credential   bool
		wantCode     errorcode.ErrorCode
		wantRequests bool
	}{
		{"bucket_only", `{"bucket":"probe-bucket"}`, false, errorcode.ErrorCode_MasterParamsError, false},
		{"prefix_only", `{"prefix":"probe/"}`, false, errorcode.ErrorCode_MasterParamsError, false},
		{"bucket_and_prefix", `{"bucket":"probe-bucket","prefix":"probe/"}`, false, errorcode.ErrorCode_MasterParamsError, false},
		{"empty_reference", `{"bucket":"probe-bucket","prefix":"probe/","cred_ref":""}`, false, errorcode.ErrorCode_MasterParamsError, false},
		{"null_reference", `{"bucket":"probe-bucket","prefix":"probe/","cred_ref":null}`, false, errorcode.ErrorCode_MasterParamsError, false},
		{"missing_file", `{"bucket":"probe-bucket","prefix":"probe/","cred_ref":"tenant"}`, false, errorcode.ErrorCode_MasterInternalError, false},
		{"backend_denied", `{"bucket":"probe-bucket","prefix":"probe/","cred_ref":"tenant"}`, true, errorcode.ErrorCode_MasterInternalError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, db, _ := newVolumeTestEngine(t)
			var requests atomic.Int32
			var writes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				requests.Add(1)
				if req.Method == http.MethodPut || req.Method == http.MethodPost || req.Method == http.MethodDelete {
					writes.Add(1)
				}
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code><Message>denied</Message></Error>`))
			}))
			t.Cleanup(srv.Close)
			dir := t.TempDir()
			credentialsDir := filepath.Join(dir, "credentials")
			require.NoError(t, os.Mkdir(credentialsDir, 0o700))
			configPath := filepath.Join(dir, "volume-s3.conf")
			config := "BUCKET=legacy-bucket\nENDPOINT=" + srv.URL + "\nREGION=us-east-1\nADDRESSING_STYLE=path\n"
			require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))
			if tc.credential {
				credential := "ACCESS_KEY_ID=fake-access-key\nSECRET_ACCESS_KEY=fake-secret-key\nENDPOINT=" + srv.URL + "\nREGION=us-east-1\nADDRESSING_STYLE=path\n"
				require.NoError(t, os.WriteFile(filepath.Join(credentialsDir, "tenant.conf"), []byte(credential), 0o600))
			}
			t.Setenv("CUBE_S3_CONFIG", configPath)
			t.Setenv("CUBE_S3_CREDENTIALS_DIR", credentialsDir)
			p := binaryplugin.New("real-s3-errors", binaryPath)
			plugin.Register(p)
			t.Cleanup(func() { plugin.UnregisterForTest(p.Name()) })
			volumeID := "vol-s3-kind-" + tc.name
			_, env := doVolumeRequest(t, r, http.MethodPost, "/cube/volume", volumeCreateBody(t, volumeID, p.Name(), tc.input))
			assert.Equal(t, int(tc.wantCode), env.Ret.RetCode, "response: %s", env.Ret.RetMsg)
			assert.Nil(t, env.Volume)
			if tc.wantCode == errorcode.ErrorCode_MasterParamsError {
				assert.Contains(t, env.Ret.RetMsg, "dynamic private_data requires cred_ref")
				assert.NotContains(t, env.Ret.RetMsg, dir)
				assert.NotContains(t, env.Ret.RetMsg, binaryPath)
			}
			if tc.name == "missing_file" {
				assert.Contains(t, env.Ret.RetMsg, `credential reference "tenant" is unreadable`)
			}
			if tc.wantRequests {
				assert.Positive(t, requests.Load(), "backend fault must actually reach the fake store")
			} else {
				assert.Zero(t, requests.Load(), "input/configuration failures must precede object-store access")
			}
			assert.Zero(t, writes.Load(), "failed create must not mutate storage")
			var count int64
			require.NoError(t, db.Model(&models.VolumeRecord{}).Count(&count).Error)
			assert.Zero(t, count, "failed create must not retain a volume row")
			_, got := doVolumeRequest(t, r, http.MethodGet, "/cube/volume/"+volumeID, "")
			assert.Equal(t, int(errorcode.ErrorCode_NotFound), got.Ret.RetCode)
		})
	}
}

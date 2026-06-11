// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

// caRootDir is the host-side install location of the CubeEgress root
// CA on the master node — same path the data plane (CubeEgress nginx)
// reads, so the source of truth is single. compute nodes pull from
// here via /cube/ca/<filename>.
//
// Hardcoded by the same reasoning as design/cube-egress-ca-bake.md
// §D2: a per-deployment override would let the master serve one CA
// while the data plane uses another, silently breaking trust.
const caRootDir = "/etc/cube/ca"

// caServableFiles is the closed allow-list of filenames this endpoint
// will serve. The path traversal check below would also catch any
// attempt to escape /etc/cube/ca, but an explicit allow-list documents
// exactly what's exposed and keeps the policy in code rather than
// implicit in os.Open behavior.
var caServableFiles = map[string]struct{}{
	"cube-root-ca.crt": {},
	"cube-root-ca.key": {},
}

// handleCADownloadAction serves the CubeEgress root CA materials
// (cube-root-ca.crt / cube-root-ca.key) to compute nodes that need to
// run their own CubeEgress instance against templates baked with the
// same CA.
//
// Path: /cube/ca/<filename>. Other names → 404.
//
// Auth: NONE today. Anyone reachable on the master HTTP port can pull
// the MITM private key. Acceptable iff the master HTTP port is
// reachable only from inside the cluster network (the typical
// one-click deployment). Production hardening — bearer token in a
// header, request-source ACL, or mTLS — should land before this
// endpoint is exposed to anything wider; a verifyAuth(r) hook is the
// natural place to add it.
func handleCADownloadAction(w http.ResponseWriter, r *http.Request, rt *CubeLog.RequestTrace) interface{} {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterParamsError),
				RetMsg:  http.StatusText(http.StatusMethodNotAllowed),
			},
		}
	}

	// Trim and basename so a malicious caller can't request
	// /cube/ca/../../etc/passwd. filepath.Base alone normalises any
	// traversal attempt to a single path element; the allow-list then
	// rejects anything not on the documented list.
	requested := filepath.Base(filepath.Clean(r.URL.Path))
	if _, ok := caServableFiles[requested]; !ok {
		return &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_NotFound),
				RetMsg:  http.StatusText(http.StatusNotFound),
			},
		}
	}

	fullPath := filepath.Join(caRootDir, requested)
	f, err := os.Open(fullPath) // #nosec G304 — file name is from a closed allow-list
	if err != nil {
		// If the master itself doesn't have the CA on disk this is an
		// operational misconfiguration, not a client error. Surfacing
		// 404 rather than 500 keeps the failure noisy on the puller's
		// side (it can choose to retry later, e.g. while the operator
		// runs cube-egress-prepare.sh on the master).
		retCode := int(errorcode.ErrorCode_MasterInternalError)
		if errors.Is(err, os.ErrNotExist) {
			retCode = int(errorcode.ErrorCode_NotFound)
		}
		return &types.Res{
			Ret: &types.Ret{
				RetCode: retCode,
				RetMsg:  err.Error(),
			},
		}
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return &types.Res{
			Ret: &types.Ret{
				RetCode: int(errorcode.ErrorCode_MasterInternalError),
				RetMsg:  err.Error(),
			},
		}
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	if r.Method == http.MethodHead {
		rt.RetCode = int64(errorcode.ErrorCode_Success)
		return nil
	}
	// http.ServeContent gives us range support for free; the CA files
	// are tiny (sub-kilobyte) so range isn't load-bearing, but it
	// keeps the response handling consistent with the artifact
	// download endpoint.
	http.ServeContent(w, r, requested, stat.ModTime(), f)
	rt.RetCode = int64(errorcode.ErrorCode_Success)
	return nil
}

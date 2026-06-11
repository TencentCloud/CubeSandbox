// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/templatecenter/cube_egress_ca"
	CubeLog "github.com/tencentcloud/CubeSandbox/cubelog"
)

// cubeEgressCAPath is the hardcoded host-side install location of the
// CubeEgress root CA. Same file the data plane reads (see
// CubeEgress/nginx.conf cert_signer.bootstrap), so a rotation done by
// `up-cube-egress.sh` automatically affects both the data plane and
// freshly-built templates without any extra configuration.
//
// Deliberately a constant, not a config knob: the bake decision is
// per-request via CreateTemplateFromImageReq.WithCubeCA. There's no
// scenario where the *path* would differ from this canonical one
// while the user still wants the bake to happen — if there is, the
// project has bigger problems than a config flag.
const cubeEgressCAPath = "/etc/cube/ca/cube-root-ca.crt"

// resolveWithCubeCA applies the "default true" rule to a *bool from
// the wire. nil (caller didn't set it) is treated as true; explicit
// true/false propagate. Centralised here so every caller agrees on
// the default and so the rule is unit-testable.
func resolveWithCubeCA(p *bool) bool {
	if p == nil {
		return true
	}
	return *p
}

// loadCubeEgressCA returns the CA PEM and its fingerprint when
// withCubeCA is true. Hard-errors on any of:
//
//   - file missing at cubeEgressCAPath
//   - file unreadable
//   - file not a valid single-CERTIFICATE PEM
//
// withCubeCA=false is a no-op: returns (nil, "", nil) so the caller
// can skip both the rootfs bake and the fingerprint folding without
// any branches of its own.
//
// The returned fingerprint feeds buildTemplateSpecFingerprintWithCA
// so a CA rotation invalidates the artifact reuse cache automatically.
func loadCubeEgressCA(ctx context.Context, withCubeCA bool) ([]byte, string, error) {
	return loadCubeEgressCAFromPath(ctx, withCubeCA, cubeEgressCAPath)
}

// loadCubeEgressCAFromPath is the testable kernel of loadCubeEgressCA.
// Production code calls loadCubeEgressCA which fixes path to the
// canonical install location; tests call this variant directly with
// a temp path so they don't need to monkey-patch a global.
func loadCubeEgressCAFromPath(ctx context.Context, withCubeCA bool, caPath string) ([]byte, string, error) {
	if !withCubeCA {
		CubeLog.WithContext(ctx).Infof(
			"cube_egress CA bake disabled by request (with_cube_ca=false); template will not carry the sandbox trust root")
		return nil, "", nil
	}
	pemBytes, err := os.ReadFile(caPath) // #nosec G304 — path is either the hardcoded const or test-supplied
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", fmt.Errorf(
				"with_cube_ca=true but CubeEgress root CA is missing at %s; "+
					"deploy CubeEgress (or copy the CA into place) before creating templates that bake it",
				caPath)
		}
		return nil, "", fmt.Errorf("read cube_egress CA %s: %w", caPath, err)
	}
	fp, err := cube_egress_ca.FingerprintOf(pemBytes)
	if err != nil {
		return nil, "", fmt.Errorf("validate cube_egress CA at %s: %w", caPath, err)
	}
	return pemBytes, fp, nil
}

// applyCubeEgressCAToRootfs runs cube_egress_ca.Bake against rootfsDir.
// When pemBytes is empty (loadCubeEgressCA returned no-op for
// with_cube_ca=false), this is a no-op and returns a zero Result.
//
// When pemBytes is non-empty, the caller has explicitly asked for the
// CA to land in the rootfs, so we hard-error if the bake produces zero
// writes. Distroless / scratch images that have neither a ca-bundle
// nor any anchor dir fail the build under with_cube_ca=true; that's
// the contract — the user asked for trust to be installed, the image
// can't accept it, the request is unsatisfiable.
func applyCubeEgressCAToRootfs(ctx context.Context, rootfsDir string, pemBytes []byte, fingerprint string) (cube_egress_ca.Result, error) {
	if len(pemBytes) == 0 {
		return cube_egress_ca.Result{Fingerprint: fingerprint}, nil
	}
	res, err := cube_egress_ca.Bake(rootfsDir, pemBytes)
	if err != nil {
		return res, fmt.Errorf("bake cube_egress CA into rootfs: %w", err)
	}
	if !res.Baked {
		return res, fmt.Errorf(
			"with_cube_ca=true but the image rootfs has no ca-bundle file and no anchor directory; "+
				"distroless / scratch-style images cannot host the trust root. reasons=%v",
			res.SkippedReasons)
	}
	CubeLog.WithContext(ctx).Infof(
		"cube_egress CA baked into rootfs: targets_written=%d fingerprint=%s",
		res.TargetsWritten, res.Fingerprint)
	return res, nil
}

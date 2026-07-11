# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

"""Run Go under a restricted egress network policy in a Cube Sandbox.

Differentiated scenario (requirement 3: "出口网络策略受限下的运行").
Cube Sandbox enforces outbound policy at the Cubelet tap network layer (kernel
level), so it cannot be bypassed from inside the VM. This script shows:

  A. Air-gapped, zero-dependency build/test
     A Go module that only imports the standard library compiles and unit-tests
     fully OFFLINE. This is Go's key strength for regulated / egress-restricted
     environments: no network == no supply-chain exposure.

  B. Egress is really cut (not cosmetic)
     Under the same air-gap, `go mod download` of an external module fails --
     evidence the policy is enforced. Dependencies require either a reachable
     private proxy (allowlist) or general internet.

  C. Allowlist mode for dependencies (opt-in)
     Switch to allowlist mode and permit ONLY your module proxy (+ DNS) CIDR so
     `go mod download` works while everything else stays blocked. Enable by
     exporting CUBE_GOPROXY_CIDRS (e.g. "10.0.1.5/32,10.0.0.53/32"). The
     sandbox GOPROXY must point at that reachable proxy (CUBE_GOPROXY_URL).

Prereqs: a Go template (see README), `pip install -r requirements.txt`,
and `CUBE_TEMPLATE_ID` set in the environment.
"""

from __future__ import annotations

import os
import sys

from e2b_code_interpreter import Sandbox

from env_utils import load_local_dotenv

load_local_dotenv()

template_id = os.environ["CUBE_TEMPLATE_ID"]

# GOTOOLCHAIN=local: never auto-download a toolchain; GOFLAGS=-mod=readonly:
# never touch the network for module graph resolution. Together these make a
# stdlib-only build run fully offline even if GOPROXY is reachable.
OFFLINE_ENV = "GOTOOLCHAIN=local GOFLAGS=-mod=readonly"

GOMOD = "module hellogo\n\ngo 1.23\n"

MAIN_GO = """package main

import (
\t"fmt"
\t"strings"
)

// JoinTags uppercases and joins tags with a comma. Pure stdlib, no deps.
func JoinTags(tags ...string) string {
\tfor i, t := range tags {
\t\ttags[i] = strings.ToUpper(t)
\t}
\treturn strings.Join(tags, ",")
}

func main() {
\tfmt.Println(JoinTags("go", "sandbox"))
}
"""

MAIN_TEST_GO = """package main

import "testing"

func TestJoinTags(t *testing.T) {
\tif got := JoinTags("go", "sandbox"); got != "GO,SANDBOX" {
\t\tt.Fatalf("JoinTags = %q; want GO,SANDBOX", got)
\t}
}
"""

# A module requiring an external dependency -- used to prove egress is cut.
DEP_GOMOD = "module needsdep\n\ngo 1.23\n\nrequire rsc.io/quote v1.5.2\n"
DEP_MAIN_GO = """package main

import (
\t"fmt"
\t"rsc.io/quote"
)

func main() { fmt.Println(quote.Hello()) }
"""


def run_air_gapped(sandbox: Sandbox) -> int:
    print("== [A] Air-gapped zero-dependency build/test ==")
    sandbox.files.make_dir("/workspace/hellogo")
    sandbox.files.write("/workspace/hellogo/go.mod", GOMOD)
    sandbox.files.write("/workspace/hellogo/main.go", MAIN_GO)
    sandbox.files.write("/workspace/hellogo/main_test.go", MAIN_TEST_GO)

    res = sandbox.commands.run(
        f"{OFFLINE_ENV} go test -v ./...", cwd="/workspace/hellogo"
    )
    print(res.stdout)
    if res.stderr:
        print(res.stderr, file=sys.stderr)
    if res.exit_code != 0:
        print("FAILED: air-gapped go test", file=sys.stderr)
        return 1
    print("air-gapped go test passed \u2713")

    # Prove egress is actually cut at the tap layer.
    res = sandbox.commands.run(
        "curl -s --max-time 3 https://proxy.golang.org -o /dev/null "
        "-w '%{http_code}' || echo blocked"
    )
    blocked = res.stdout.strip() == "blocked" or res.exit_code != 0
    print("public egress blocked:", blocked)
    if not blocked:
        print("FAILED: egress was NOT restricted", file=sys.stderr)
        return 1
    return 0


def run_dep_blocked(sandbox: Sandbox) -> int:
    print("\n== [B] External dependency is blocked under air-gap ==")
    sandbox.files.make_dir("/workspace/needsdep")
    sandbox.files.write("/workspace/needsdep/go.mod", DEP_GOMOD)
    sandbox.files.write("/workspace/needsdep/main.go", DEP_MAIN_GO)
    res = sandbox.commands.run("go mod download", cwd="/workspace/needsdep")
    blocked = res.exit_code != 0
    print((res.stderr or res.stdout).strip())
    print("external go mod download blocked:", blocked)
    if not blocked:
        print("FAILED: dependency download succeeded despite air-gap", file=sys.stderr)
        return 1
    return 0


def run_with_proxy_allowlist() -> int:
    """Allowlist mode: permit ONLY a module proxy (+ DNS), block the rest.

    Enable by exporting CUBE_GOPROXY_CIDRS (comma-separated CIDRs), e.g. the
    resolved CIDR(s) of your private GOPROXY and your DNS server. The sandbox
    GOPROXY must point at that reachable proxy (CUBE_GOPROXY_URL).
    """
    raw = os.environ.get("CUBE_GOPROXY_CIDRS", "").strip()
    if not raw:
        print("\n== [C] Allowlist for deps: SKIPPED ==")
        print(
            "Set CUBE_GOPROXY_CIDRS to enable (e.g. '10.0.1.5/32,10.0.0.53/32')."
        )
        return 0

    allow_out = [c.strip() for c in raw.split(",") if c.strip()]
    proxy_url = os.environ.get("CUBE_GOPROXY_URL", "direct")
    print(f"\n== [C] Allowlist for deps: allow_out={allow_out} GOPROXY={proxy_url} ==")
    with Sandbox.create(
        template=template_id,
        allow_internet_access=False,
        network={"allow_out": allow_out},
    ) as sandbox:
        sandbox.files.make_dir("/workspace/needsdep")
        sandbox.files.write("/workspace/needsdep/go.mod", DEP_GOMOD)
        sandbox.files.write("/workspace/needsdep/main.go", DEP_MAIN_GO)
        res = sandbox.commands.run(
            f"GOPROXY={proxy_url} GOFLAGS=-mod=mod go mod download",
            cwd="/workspace/needsdep",
        )
        print(res.stdout)
        if res.stderr:
            print(res.stderr, file=sys.stderr)
        if res.exit_code != 0:
            print("FAILED: allowlist go mod download", file=sys.stderr)
            return 1
        print("allowlist go mod download passed \u2713")
    return 0


def main() -> int:
    rc = 0
    with Sandbox.create(template=template_id, allow_internet_access=False) as sandbox:
        rc |= run_air_gapped(sandbox)
        rc |= run_dep_blocked(sandbox)
    rc |= run_with_proxy_allowlist()
    return rc


if __name__ == "__main__":
    raise SystemExit(main())

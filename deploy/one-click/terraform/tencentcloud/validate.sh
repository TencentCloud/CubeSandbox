#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Self-contained static validation for the Tencent Cloud one-click Terraform
# deployer. Runs against cloud-free, local checks (no cloud credentials needed),
# so it is safe to wire into CI:
#   - bash -n syntax check on every shell script (always runs, no deps)
#   - terraform fmt -check -recursive
#   - terraform init -backend=false  (+ terraform validate)
#   - shellcheck on the deployer's shell scripts (skipped if not installed)
#
# terraform/shellcheck are skipped when not installed. Set REQUIRE_TOOLS=1 (e.g.
# in CI) to turn a missing tool into a failure instead of a silent skip.
#
# Usage: ./validate.sh   (or: REQUIRE_TOOLS=1 ./validate.sh)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

fail=0
require_tools="${REQUIRE_TOOLS:-0}"

# Report a missing optional tool: fail in REQUIRE_TOOLS mode, otherwise skip.
#   $1 = tool name, $2 = description of what is skipped
missing_tool() {
	if [ "${require_tools}" = "1" ] || [ "${require_tools}" = "true" ]; then
		echo "==> $1 not found and REQUIRE_TOOLS is set; failing" >&2
		fail=1
	else
		echo "==> $1 not found; skipping $2" >&2
	fi
}

# Always parse every shell script (needs no external tools), so syntax breakage
# is caught even when shellcheck is unavailable.
echo "==> bash -n (syntax check) on ./*.sh"
for f in ./*.sh; do
	bash -n "$f" || {
		echo "    syntax error in $f"
		fail=1
	}
done

if command -v terraform >/dev/null 2>&1; then
	echo "==> terraform fmt -check -recursive"
	terraform fmt -check -recursive || {
		echo "    fix with: terraform fmt -recursive"
		fail=1
	}

	echo "==> terraform init -backend=false"
	# Provider plugins are pulled from registry.terraform.io on every run (the
	# lock file and .terraform/ cache are gitignored), and that fetch — the
	# tencentcloud provider in particular — intermittently times out and fails
	# an otherwise-green run. Give the registry client a longer timeout and more
	# internal retries, then wrap init in an outer retry loop with linear
	# backoff so a transient network blip doesn't break validation. All three
	# knobs are overridable from the environment for slower/faster networks.
	export TF_REGISTRY_CLIENT_TIMEOUT="${TF_REGISTRY_CLIENT_TIMEOUT:-30}"
	export TF_REGISTRY_DISCOVERY_RETRY="${TF_REGISTRY_DISCOVERY_RETRY:-3}"
	init_attempts="${TF_INIT_ATTEMPTS:-3}"
	# Guard init so a final failure records fail=1 and skips validate, instead
	# of aborting the whole script via `set -e` before the summary line prints.
	init_ok=0
	for attempt in $(seq 1 "${init_attempts}"); do
		if terraform init -backend=false -input=false >/dev/null; then
			init_ok=1
			break
		fi
		if [ "${attempt}" -lt "${init_attempts}" ]; then
			echo "    terraform init attempt ${attempt}/${init_attempts} failed; retrying in $((attempt * 5))s..." >&2
			sleep "$((attempt * 5))"
		fi
	done

	if [ "${init_ok}" = "1" ]; then
		# tke-addons.tf reads webui-nginx.conf via file(); that file is generated
		# at build/run time and is gitignored, so drop in a throwaway placeholder
		# (with the two upstream tokens the config expects) just for validate.
		placeholder=0
		if [ ! -f webui-nginx.conf ]; then
			placeholder=1
			printf 'server { location /cubeapi/ { proxy_pass __WEB_UI_UPSTREAM__; } location /sandbox/ { proxy_pass __SANDBOX_PROXY_UPSTREAM__; } }\n' \
				>webui-nginx.conf
		fi
		echo "==> terraform validate"
		terraform validate || fail=1
		[ "${placeholder}" = "1" ] && rm -f webui-nginx.conf
	else
		echo "    terraform init failed after ${init_attempts} attempts; skipping validate" >&2
		fail=1
	fi
else
	missing_tool terraform "fmt/validate"
fi

if command -v shellcheck >/dev/null 2>&1; then
	echo "==> shellcheck (severity=error)"
	# SC1091: lib-state-sync.sh is sourced at runtime; its path is resolvable, but
	# keep this robust across checkout layouts. Gate on errors so genuine bugs
	# fail the build without churning on style-level info/warnings.
	shellcheck -x -e SC1091 --severity=error ./*.sh || fail=1
else
	missing_tool shellcheck "shell lint"
fi

if [ "${fail}" != "0" ]; then
	echo "validate.sh: FAILED"
	exit 1
fi
echo "validate.sh: OK"

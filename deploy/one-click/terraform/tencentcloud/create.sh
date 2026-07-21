#!/usr/bin/env bash
#
# create.sh — provision a clustered CubeSandbox on Tencent Cloud (TKE control
# plane + CVM compute nodes) with Terraform. Run it from inside an extracted
# release bundle (or the source tree). Tear everything down with destroy.sh.
#
# Required credentials:
#   TENCENTCLOUD_SECRET_ID / TENCENTCLOUD_SECRET_KEY   (https://console.cloud.tencent.com/cam/capi)
#
# Common configuration variables are documented in env.example. The ADVANCED
# toggles below are env-only (not prompted); defaults are shown in parentheses:
#
#   TENCENTCLOUD_VERBOSE                 verbose terraform logs (interactive: off, non-interactive: on)
#   TENCENTCLOUD_BUILD_IMAGES            build+push the four component images on the jumpserver (1); 0 reuses pushed images
#   TENCENTCLOUD_REINSTALL               force re-run the compute-node install even if already installed (0)
#   TENCENTCLOUD_RESET_DB                drop+recreate the cube database on this run (0)
#   TENCENTCLOUD_ALLOW_INSECURE_DEFAULTS allow the built-in demo passwords on a non-interactive run (0)
#   TENCENTCLOUD_LOCAL_BUNDLE            path to cube-sandbox-one-click-*.tar.gz (auto-detected inside a bundle)
#   TENCENTCLOUD_PVM_KERNEL_VMLINUX      path to vmlinux-pvm (only if the bundle ships none)
#   TENCENTCLOUD_PVM_KERNEL_RPM_URL      PVM kernel RPM URL (OpenCloudOS mirror default)
#   TENCENTCLOUD_SSH_PORT                SSH port on compute nodes through the jumpserver (22)
#   TENCENTCLOUD_SSH_PRIVATE_KEY_PATH    SSH private key (default ./.ssh/id_rsa, auto-generated)
#   TENCENTCLOUD_SSH_PUBLIC_KEY_PATH     SSH public key  (default ./.ssh/id_rsa.pub)
#   TENCENTCLOUD_JUMPSERVER_SSH_WAIT     jumpserver SSH readiness poll iterations (200)
#
# Usage: ./create.sh [-h|--help]

set -euo pipefail

# Harden permissions for everything this deployer writes locally — Terraform
# state (holds DB/Redis/TCR/CA secrets), the plan/apply logs, the saved .env,
# the generated SSH key, the kubeconfig and the cube-proxy TLS material — so they
# default to owner-only and are not world-readable on a shared/build host.
umask 077

# Print the documented header above (shebang line through the first blank line).
_usage() {
	sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^#\{1,\} \{0,1\}//;s/^#$//'
}

case "${1:-}" in
-h | --help)
	_usage
	exit 0
	;;
esac

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

# Ensure the .kube directory exists (required when the kubernetes provider initializes)
mkdir -p "$SCRIPT_DIR/.kube"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

# Reconcile local Terraform state with the real cloud environment
# (refresh + import of out-of-band stateful resources). See the file
# header for the full rationale.
# shellcheck source=./lib-state-sync.sh
source "${SCRIPT_DIR}/lib-state-sync.sh"

# Pure phase/rerun decision helpers (no cloud calls) for the phased apply state
# machine; unit-tested by deploy/one-click/tests/test_phase_flags.sh. This also
# provides _set_phase_flags (the producer→consumer tuple parser), kept beside the
# phase_*_flags producers so the round-trip is covered by that same dry-run test.
# shellcheck source=./lib-phases.sh
source "${SCRIPT_DIR}/lib-phases.sh"

# Global state
VERBOSE=0
BUNDLE_UPDATED=0
# Whether the component images were built+pushed (or reused) successfully. When 0,
# the TKE addons deployment and the follow-up health checks are skipped.
IMAGES_OK=1
# Whether the image build/tag were already configured (env vars or saved
# selection) before prompt_deployment_env ran. When 1, tcr_build_and_push skips
# the interactive "build & push?" confirmation and just reminds.
IMAGES_CONFIGURED=0
JUMP_PROXY_OPTS=()

# List of available instance types (used for auto-fallback when creation fails)
CVM_TYPES=()
CVM_CPUS=()
CVM_MEMS=()
CVM_TYPE_INDEX=-1

# Jumpserver instance types (auto-fallback when creation fails)
JUMPSERVER_TYPES=()
JUMPSERVER_CPUS=()
JUMPSERVER_MEMS=()
JUMPSERVER_TYPE_INDEX=-1
JUMPSERVER_ZONE_INDEX=0
COMPUTE_ZONE_INDEX=0
TKE_ZONE_INDEX=0

# step2_apply parametrization:
#   STEP2_TARGETS — when non-empty, the apply is restricted to these resource
#                   addresses (terraform -target=...), so the orchestrator can
#                   provision one phase at a time and wait for it synchronously.
#                   Empty means a full apply.
#   STEP2_LABEL        — the banner shown for the apply step.
#   STEP2_CVM_FALLBACK — when 1, auto-fallback cycles instance types and
#                        per-role availability zones (same VPC) on stock errors.
STEP2_TARGETS=()
STEP2_LABEL="Step: terraform apply (create CVM)"
STEP2_CVM_FALLBACK=0
# When >= 0, step2_apply is purchasing a single compute node at this index.
STEP2_COMPUTE_NODE_INDEX=-1

# Per-node compute cluster state (actual purchased config; env type is preference only)
COMPUTE_PREFERRED_TYPE=""
COMPUTE_PREFERRED_ZONE=""
COMPUTE_PURCHASED_TYPES=()
COMPUTE_PURCHASED_ZONES=()

banner() {
	echo ""
	echo -e "${CYAN}============================================================${NC}"
	echo -e "${CYAN}  $1${NC}"
	echo -e "${CYAN}============================================================${NC}"
	echo ""
}

# _display_width and _draw_box are shared helpers sourced from lib-state-sync.sh.

# Terraform command wrapper: silent by default, shows logs when TENCENTCLOUD_VERBOSE=1
_tf() {
	if [ "${VERBOSE}" = "1" ]; then
		terraform "$@"
	else
		terraform "$@" >/dev/null 2>&1
	fi
}

# Same as _tf but keeps stderr for error diagnostics
_tf_keep_stderr() {
	if [ "${VERBOSE}" = "1" ]; then
		terraform "$@"
	else
		terraform "$@" >/dev/null
	fi
}

# ---------------------------------------------------------------
# ensure_terraform — make sure the terraform CLI is available
#   If terraform is already on PATH, do nothing. Otherwise download the pinned
#   release zip from releases.hashicorp.com and unzip it into a suitable bin dir:
#     - /usr/local/bin when writable (root), otherwise ${SCRIPT_DIR}/.bin which is
#       prepended to PATH for the rest of this run.
# ---------------------------------------------------------------
# _release_platform — OS slug used by HashiCorp / jq release artifacts (linux|darwin).
_release_platform() {
	case "$(uname -s)" in
		Linux) echo linux ;;
		Darwin) echo darwin ;;
		*)
			echo -e "${RED}✗ Unsupported OS for auto-install: $(uname -s); install terraform/jq manually${NC}" >&2
			exit 1
			;;
	esac
}

TERRAFORM_VERSION="${TERRAFORM_VERSION:-1.15.6}"
TERRAFORM_PARALLELISM="${TERRAFORM_PARALLELISM:-15}"
ensure_terraform() {
	if command -v terraform >/dev/null 2>&1; then
		return 0
	fi

	echo -e "${YELLOW}terraform not found, installing v${TERRAFORM_VERSION}...${NC}"

	# Map uname arch to the naming used by HashiCorp release artifacts
	local arch os
	os="$(_release_platform)"
	case "$(uname -m)" in
		x86_64 | amd64) arch="amd64" ;;
		aarch64 | arm64) arch="arm64" ;;
		*)
			echo -e "${RED}✗ Unsupported architecture: $(uname -m); please install terraform manually${NC}"
			exit 1
			;;
	esac

	# Choose an install dir: prefer /usr/local/bin, fall back to a local dir on PATH
	local bin_dir
	if [ -w /usr/local/bin ] 2>/dev/null; then
		bin_dir="/usr/local/bin"
	else
		bin_dir="${SCRIPT_DIR}/.bin"
		mkdir -p "${bin_dir}"
		export PATH="${bin_dir}:${PATH}"
	fi

	local url="https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/terraform_${TERRAFORM_VERSION}_${os}_${arch}.zip"
	local tmp_zip
	tmp_zip="$(mktemp -t terraform.XXXXXX.zip)"

	if command -v wget >/dev/null 2>&1; then
		wget -q -O "${tmp_zip}" "${url}" || {
			echo -e "${RED}✗ Failed to download terraform from ${url}${NC}"
			rm -f "${tmp_zip}"
			exit 1
		}
	elif command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "${tmp_zip}" "${url}" || {
			echo -e "${RED}✗ Failed to download terraform from ${url}${NC}"
			rm -f "${tmp_zip}"
			exit 1
		}
	else
		echo -e "${RED}✗ Neither wget nor curl is available to download terraform${NC}"
		rm -f "${tmp_zip}"
		exit 1
	fi

	unzip -o "${tmp_zip}" terraform -d "${bin_dir}" >/dev/null || {
		echo -e "${RED}✗ Failed to unzip terraform into ${bin_dir}${NC}"
		rm -f "${tmp_zip}"
		exit 1
	}
	chmod +x "${bin_dir}/terraform"
	rm -f "${tmp_zip}"
	hash -r 2>/dev/null || true

	if ! command -v terraform >/dev/null 2>&1; then
		echo -e "${RED}✗ terraform installation failed (${bin_dir} not on PATH?)${NC}"
		exit 1
	fi
	echo -e "${GREEN}✓ terraform installed: $(command -v terraform) ($(terraform version | head -n1))${NC}"
}

# ---------------------------------------------------------------
# ensure_jq — make sure the jq CLI is available on the control host.
#   jq is a hard dependency: the script parses `terraform output -json` (compute
#   node IPs, config summary, cube-master node list, …) with it. When jq is
#   missing every `... | jq ...` silently falls back to 0/empty — e.g. Step 8
#   would wrongly report "compute node private IPs could not be read" even though
#   the nodes exist. Install it via the system package manager, falling back to a
#   static binary download into ${SCRIPT_DIR}/.bin (prepended to PATH).
# ---------------------------------------------------------------
JQ_VERSION="${JQ_VERSION:-1.7.1}"
ensure_jq() {
	if command -v jq >/dev/null 2>&1; then
		return 0
	fi

	echo -e "${YELLOW}jq not found, installing...${NC}"

	# 1) Prefer the system package manager (works offline against cloud mirrors).
	local pm
	for pm in brew dnf yum apt-get zypper apk; do
		command -v "$pm" >/dev/null 2>&1 || continue
		case "$pm" in
			brew) "$pm" install jq >/dev/null 2>&1 || true ;;
			apt-get) DEBIAN_FRONTEND=noninteractive "$pm" install -y jq >/dev/null 2>&1 || true ;;
			apk) "$pm" add --no-cache jq >/dev/null 2>&1 || true ;;
			*) "$pm" install -y jq >/dev/null 2>&1 || true ;;
		esac
		if command -v jq >/dev/null 2>&1; then
			hash -r 2>/dev/null || true
			echo -e "${GREEN}✓ jq installed: $(command -v jq) ($(jq --version 2>/dev/null))${NC}"
			return 0
		fi
	done

	# 2) Fall back to a static jq binary from GitHub releases.
	local jq_arch os jq_os
	os="$(_release_platform)"
	case "$os" in
		linux) jq_os="linux" ;;
		darwin) jq_os="macos" ;;
	esac
	case "$(uname -m)" in
		x86_64 | amd64) jq_arch="amd64" ;;
		aarch64 | arm64) jq_arch="arm64" ;;
		*)
			echo -e "${RED}✗ Unsupported architecture for jq auto-install: $(uname -m); please install jq manually${NC}"
			exit 1
			;;
	esac

	local bin_dir
	if [ -w /usr/local/bin ] 2>/dev/null; then
		bin_dir="/usr/local/bin"
	else
		bin_dir="${SCRIPT_DIR}/.bin"
		mkdir -p "${bin_dir}"
		export PATH="${bin_dir}:${PATH}"
	fi

	local url="https://github.com/jqlang/jq/releases/download/jq-${JQ_VERSION}/jq-${jq_os}-${jq_arch}"
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "${bin_dir}/jq" "${url}" || true
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "${bin_dir}/jq" "${url}" || true
	fi
	chmod +x "${bin_dir}/jq" 2>/dev/null || true
	hash -r 2>/dev/null || true

	if ! command -v jq >/dev/null 2>&1; then
		echo -e "${RED}✗ jq installation failed; please install jq manually (e.g. dnf install -y jq) and re-run${NC}"
		exit 1
	fi
	echo -e "${GREEN}✓ jq installed: $(command -v jq) ($(jq --version 2>/dev/null))${NC}"
}

# ---------------------------------------------------------------
# _autodetect_local_bundle — automatically locate the outer bundle tar package inside the extracted release bundle
#   create.sh is located at <bundle>/terraform/tencentcloud/, so the bundle root is two levels up.
#   Only takes effect when actually inside an extracted release bundle (containing assets/package/sandbox-package.tar.gz);
#   when running directly from the source tree, keep the original online install behavior (return an empty string).
#   The found tar package path is printed to stdout; human-readable info always goes to stderr to avoid polluting the return value.
# ---------------------------------------------------------------
_autodetect_local_bundle() {
	local bundle_root
	bundle_root="$(cd "${SCRIPT_DIR}/../.." 2>/dev/null && pwd)" || return 0
	[ -n "${bundle_root}" ] || return 0
	# Must be inside an extracted release bundle: sandbox-package.tar.gz is the input for jumpserver-side build/distribution
	[ -f "${bundle_root}/assets/package/sandbox-package.tar.gz" ] || return 0

	# 1) Outer tar package with the same name as the extracted directory (most common: the user just ran `tar xzf` and the tar package is still in the same directory).
	#    Only trust an exact name match to avoid mistakenly using a different version of cube-sandbox-one-click-*.tar.gz in the same directory.
	if [ -f "${bundle_root}.tar.gz" ]; then
		printf '%s\n' "${bundle_root}.tar.gz"
		return 0
	fi

	# 2) Outer tar package not found (the user deleted/renamed the directory): repack the extracted directory in place,
	#    ensuring what is uploaded to jumpserver is exactly identical to the local extracted directory.
	#    The jumpserver-side unpack logic expects the outer structure <dir>/assets/package/sandbox-package.tar.gz,
	#    so the top-level directory must be packed together.
	local repacked="/tmp/$(basename "${bundle_root}").tar.gz"
	echo -e "  ${YELLOW}Outer bundle tar package not found, repacking the release bundle directory in place...${NC}" >&2
	if tar -C "$(dirname "${bundle_root}")" -czf "${repacked}" "$(basename "${bundle_root}")" 2>/dev/null; then
		printf '%s\n' "${repacked}"
		return 0
	fi
	echo -e "  ${YELLOW}⚠ Repack failed, will fall back to online install mode${NC}" >&2
	return 0
}

# ---------------------------------------------------------------
# select_bundle — interactively choose the deployment bundle source. Supports a
#   local .tar.gz file path or a web URL (downloaded locally and then treated as
#   a local bundle so the existing scp upload path is reused). An explicit
#   TENCENTCLOUD_LOCAL_BUNDLE skips the prompt. Sets the global LOCAL_BUNDLE; an
#   empty value means "online default" (the jumpserver downloads the bundle
#   itself). In a non-interactive shell it keeps the legacy auto-detect / online
#   fallback behaviour.
# ---------------------------------------------------------------
select_bundle() {
	if [ -n "${TENCENTCLOUD_LOCAL_BUNDLE:-}" ]; then
		# A persisted/explicit path may have been removed (e.g. a /tmp download was
		# cleared between runs). Only trust it when the file still exists; otherwise
		# fall through to auto-detect / prompt so re-runs stay robust.
		if [ -f "${TENCENTCLOUD_LOCAL_BUNDLE}" ]; then
			LOCAL_BUNDLE="${TENCENTCLOUD_LOCAL_BUNDLE}"
			echo -e "${GREEN}✓ Deployment bundle (from \$TENCENTCLOUD_LOCAL_BUNDLE): ${LOCAL_BUNDLE}${NC}"
			return 0
		fi
		echo -e "${YELLOW}⚠ \$TENCENTCLOUD_LOCAL_BUNDLE points to a missing file (${TENCENTCLOUD_LOCAL_BUNDLE}); re-resolving${NC}"
		unset TENCENTCLOUD_LOCAL_BUNDLE
	fi

	local detected
	detected="$(_autodetect_local_bundle)"

	if [ ! -t 0 ]; then
		LOCAL_BUNDLE="${detected}"
		[ -n "${LOCAL_BUNDLE}" ] && echo -e "${GREEN}✓ Automatically detected local bundle: ${LOCAL_BUNDLE}${NC}"
		return 0
	fi

	echo -e "${YELLOW}Deployment bundle (cube-sandbox-one-click-*.tar.gz):${NC}"
	if [ -n "${detected}" ]; then
		printf "  ${GREEN}%2d)${NC} %s ${CYAN}(default)${NC}\n" 1 "Use the detected local bundle: ${detected}"
	else
		printf "  ${GREEN}%2d)${NC} %s ${CYAN}(default)${NC}\n" 1 "Online default (the jumpserver downloads it automatically)"
	fi
	printf "  ${GREEN}%2d)${NC} %s\n" 2 "Enter a local file path"
	printf "  ${GREEN}%2d)${NC} %s\n" 3 "Enter a web URL (download it now)"

	local choice
	while true; do
		read -r -p "$(echo -e "${YELLOW}Select [1-3, Enter=default]: ${NC}")" choice
		case "${choice}" in
		"" | 1)
			LOCAL_BUNDLE="${detected}"
			break
			;;
		2)
			local path
			read -r -p "$(echo -e "${YELLOW}Enter the local .tar.gz path: ${NC}")" path
			path="${path/#\~/$HOME}"
			if [ -f "${path}" ]; then
				LOCAL_BUNDLE="$(cd "$(dirname "${path}")" && pwd)/$(basename "${path}")"
				break
			fi
			echo -e "${RED}File not found: ${path}; please try again${NC}"
			;;
		3)
			local url
			read -r -p "$(echo -e "${YELLOW}Enter the bundle URL (https://...tar.gz): ${NC}")" url
			if [ -z "${url}" ]; then
				echo -e "${RED}Empty URL, please try again${NC}"
				continue
			fi
			local dest
			dest="/tmp/$(basename "${url%%\?*}")"
			case "${dest}" in
			*.tar.gz) ;;
			*) dest="/tmp/cube-sandbox-bundle-download.tar.gz" ;;
			esac
			echo -e "  ${CYAN}Downloading bundle from ${url}...${NC}"
			if curl -fL --connect-timeout 15 --retry 2 "${url}" -o "${dest}"; then
				LOCAL_BUNDLE="${dest}"
				echo -e "  ${GREEN}✓ Downloaded to ${dest}${NC}"
				break
			fi
			echo -e "${RED}Download failed; please try again${NC}"
			rm -f "${dest}"
			;;
		*)
			echo -e "${RED}Invalid input, please select again${NC}"
			;;
		esac
	done

	if [ -n "${LOCAL_BUNDLE}" ]; then
		echo -e "${GREEN}✓ Deployment bundle: ${LOCAL_BUNDLE}${NC}"
	else
		echo -e "${GREEN}✓ Deployment bundle: online default (jumpserver downloads automatically)${NC}"
	fi
}

# ---------------------------------------------------------------
# _bundle_has_vmlinux_pvm — on the jumpserver, extract the local bundle, then
#   extract its inner assets/package/sandbox-package.tar.gz, and report whether
#   sandbox-package/cube-kernel-scf/vmlinux-pvm exists (and is non-empty). The
#   compute-node installer (install.sh → select_installed_kernel_vmlinux) reads
#   the PVM guest kernel from that path, so a missing file would fail the
#   install. Returns 0 when present, 1 otherwise.
# ---------------------------------------------------------------
_bundle_has_vmlinux_pvm() {
	[ -n "${LOCAL_BUNDLE:-}" ] || return 1

	local js_pub_ip key_file bundle_name
	js_pub_ip=$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "")
	key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"
	bundle_name="$(basename "${LOCAL_BUNDLE}")"
	[ -z "$js_pub_ip" ] && return 1

	local js_ssh=(
		ssh -i "${key_file}" -p 443
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
		-o ConnectTimeout=15 -o BatchMode=yes -o LogLevel=ERROR
	)

	# Upload the bundle to the jumpserver if it is not already there.
	# Send scp's output to stderr (the terminal) so its progress bar is visible
	# without polluting this function's captured stdout.
	if ! "${js_ssh[@]}" root@"${js_pub_ip}" "[ -f /tmp/${bundle_name} ]" 2>/dev/null; then
		scp -i "${key_file}" -P 443 \
			-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
			-o ConnectTimeout=15 \
			"${LOCAL_BUNDLE}" "root@${js_pub_ip}:/tmp/${bundle_name}" 1>&2 || return 1
	fi

	local out
	out=$("${js_ssh[@]}" root@"${js_pub_ip}" "
    set -e
    rm -rf /tmp/cube-vmcheck
    mkdir -p /tmp/cube-vmcheck
    tar -xzf /tmp/${bundle_name} -C /tmp/cube-vmcheck/
    BUNDLE_DIR=\$(ls -d /tmp/cube-vmcheck/*/ 2>/dev/null | head -1)
    PKG_TAR=\"\${BUNDLE_DIR}assets/package/sandbox-package.tar.gz\"
    [ -f \"\$PKG_TAR\" ] || { echo MISSING; exit 0; }
    tar -xzf \"\$PKG_TAR\" -C \"\${BUNDLE_DIR}assets/package/\"
    VM=\"\${BUNDLE_DIR}assets/package/sandbox-package/cube-kernel-scf/vmlinux-pvm\"
    if [ -s \"\$VM\" ]; then echo OK; else echo MISSING; fi
  " 2>/dev/null) || true
	echo "$out" | grep -q OK
}

# ---------------------------------------------------------------
# resolve_vmlinux_pvm — make sure a PVM guest kernel (vmlinux-pvm) is available
#   for the compute nodes. Resolution order:
#     1) explicit local file (TENCENTCLOUD_PVM_KERNEL_VMLINUX / PVM_KERNEL_VMLINUX)
#        — used as an override and injected into the bundle on each compute node;
#     2) the bundle already ships sandbox-package/cube-kernel-scf/vmlinux-pvm
#        — nothing to do, the bundled kernel is used as-is;
#     3) neither present — prompt the user for a local file path or a web URL
#        (downloaded locally) and use that as the override.
#   On a non-interactive shell with no source, it warns and continues.
# ---------------------------------------------------------------
resolve_vmlinux_pvm() {
	# Online mode (no local bundle): the install script pulls everything itself.
	[ -n "${LOCAL_BUNDLE:-}" ] || return 0

	# 1) Explicit / pre-existing local file wins (acts as an override).
	if [ -f "${PVM_KERNEL_VMLINUX}" ]; then
		echo -e "  ${GREEN}✓ vmlinux-pvm: ${PVM_KERNEL_VMLINUX} (override; injected into the bundle on compute nodes)${NC}"
		return 0
	fi

	# 2) Bundle already contains it.
	echo -e "  ${CYAN}Checking the bundle for cube-kernel-scf/vmlinux-pvm...${NC}"
	if _bundle_has_vmlinux_pvm; then
		echo -e "  ${GREEN}✓ vmlinux-pvm found inside the bundle; using the bundled kernel${NC}"
		PVM_KERNEL_VMLINUX=""
		return 0
	fi

	echo -e "  ${YELLOW}⚠ vmlinux-pvm not found in the bundle${NC}"

	# 3) No TTY → cannot prompt; warn and continue (install may fail).
	if [ ! -t 0 ]; then
		echo -e "  ${YELLOW}⚠ no interactive terminal; set TENCENTCLOUD_PVM_KERNEL_VMLINUX to provide one${NC}"
		return 0
	fi

	echo -e "${YELLOW}Select the vmlinux-pvm kernel to use:${NC}"
	printf "  ${GREEN}%2d)${NC} %s\n" 1 "Enter a local file path"
	printf "  ${GREEN}%2d)${NC} %s\n" 2 "Enter a web URL (download it now)"

	local choice
	while true; do
		read -r -p "$(echo -e "${YELLOW}Select [1-2]: ${NC}")" choice
		case "${choice}" in
		1)
			local path
			read -r -p "$(echo -e "${YELLOW}Enter the local vmlinux-pvm path: ${NC}")" path
			path="${path/#\~/$HOME}"
			if [ -s "${path}" ]; then
				PVM_KERNEL_VMLINUX="$(cd "$(dirname "${path}")" && pwd)/$(basename "${path}")"
				break
			fi
			echo -e "${RED}File not found or empty: ${path}; please try again${NC}"
			;;
		2)
			local url dest
			read -r -p "$(echo -e "${YELLOW}Enter the vmlinux-pvm URL: ${NC}")" url
			if [ -z "${url}" ]; then
				echo -e "${RED}Empty URL, please try again${NC}"
				continue
			fi
			dest="/tmp/$(basename "${url%%\?*}")"
			[ -n "$(basename "${dest}")" ] || dest="/tmp/vmlinux-pvm-download"
			echo -e "  ${CYAN}Downloading vmlinux-pvm from ${url}...${NC}"
			if curl -fL --connect-timeout 15 --retry 2 "${url}" -o "${dest}" && [ -s "${dest}" ]; then
				PVM_KERNEL_VMLINUX="${dest}"
				echo -e "  ${GREEN}✓ Downloaded to ${dest}${NC}"
				break
			fi
			echo -e "${RED}Download failed; please try again${NC}"
			rm -f "${dest}"
			;;
		*)
			echo -e "${RED}Invalid input, please select again${NC}"
			;;
		esac
	done
	echo -e "  ${GREEN}✓ vmlinux-pvm: ${PVM_KERNEL_VMLINUX}${NC}"
}

# ---------------------------------------------------------------
# prepare_webui_nginx_conf — ensure the webui-nginx.conf required by tke-addons.tf exists
#   tke-addons.tf uses file("${path.module}/webui-nginx.conf") to render cube-webui's
#   nginx config. This file is not maintained separately, but taken directly from the canonical source webui/nginx.conf:
#     - In the release bundle: build-release-bundle.sh has already placed webui/nginx.conf here;
#     - Running from the source tree: when missing, copy it from ../../webui/nginx.conf in the same repo.
# ---------------------------------------------------------------
prepare_webui_nginx_conf() {
	local dst="${SCRIPT_DIR}/webui-nginx.conf"
	[ -f "${dst}" ] && return 0

	local src="${SCRIPT_DIR}/../../webui/nginx.conf"
	if [ -f "${src}" ]; then
		cp -f "${src}" "${dst}"
		echo -e "  ${GREEN}✓ Generated webui-nginx.conf from webui/nginx.conf${NC}"
		return 0
	fi

	cat >"${dst}" <<'EOF'
worker_processes auto;
events { worker_connections 1024; }
http {
  server {
    listen 80;
    root /usr/share/nginx/html;
    index index.html;
    location ^~ /sandbox/ { proxy_pass __SANDBOX_PROXY_UPSTREAM__; }
    location /cubeapi/ { proxy_pass __WEB_UI_UPSTREAM__/cubeapi/; }
    location / { try_files $uri $uri/ /index.html; }
  }
}
EOF
	echo -e "  ${YELLOW}⚠ webui/nginx.conf source not found; generated a minimal webui-nginx.conf fallback${NC}"
	return 0
}

# ---------------------------------------------------------------
# prepare_cubeproxy_nginx_conf — ensure the cubeproxy-nginx.conf required by
#   tke-addons.tf exists. The Kubernetes deployment mounts this file so the
#   admin listener is reachable by cube-lifecycle-manager.
# ---------------------------------------------------------------
prepare_cubeproxy_nginx_conf() {
	local dst="${SCRIPT_DIR}/cubeproxy-nginx.conf"
	[ -f "${dst}" ] && return 0

	local src
	src="${SCRIPT_DIR}/../../cubeproxy/nginx.conf.template"
	if [ -f "${src}" ]; then
		cp -f "${src}" "${dst}"
		echo -e "  ${GREEN}✓ Generated cubeproxy-nginx.conf from ${src}${NC}"
		return 0
	fi

	src="${SCRIPT_DIR}/../../../../CubeProxy/nginx.conf"
	if [ -f "${src}" ]; then
		{
			cat <<'EOF'
# NOTE: This file is auto-generated from CubeProxy/nginx.conf.
# DO NOT edit by hand; modify the upstream CubeProxy/nginx.conf instead.

EOF
			sed \
				-e 's|^worker_processes [0-9]\+;|worker_processes auto;|' \
				-e 's|^\(\s*listen \)8081\( reuseport;\)|\1__CUBE_PROXY_HTTP_PORT__\2|' \
				-e 's|^\(\s*listen \)8080\( ssl reuseport;\)|\1__CUBE_PROXY_HTTPS_PORT__\2|' \
				-e 's|^\(\s*set \$host_proxy_port \)8081;|\1__CUBE_PROXY_HTTP_PORT__;|' \
				-e 's|^\(\s*set \$host_proxy_port \)8080;|\1__CUBE_PROXY_HTTPS_PORT__;|' \
				-e 's|^\(\s*listen \)127\.0\.0\.1:8082;|\1__CUBE_PROXY_ADMIN_LISTEN__:8082;|' \
				-e 's|/usr/local/openresty/nginx/certs/cube\.app+3\.pem|/usr/local/openresty/nginx/certs/__CUBE_PROXY_SSL_CERT__|' \
				-e 's|/usr/local/openresty/nginx/certs/cube\.app+3-key\.pem|/usr/local/openresty/nginx/certs/__CUBE_PROXY_SSL_KEY__|' \
				"${src}"
		} >"${dst}"
		local token
		for token in __CUBE_PROXY_HTTP_PORT__ __CUBE_PROXY_HTTPS_PORT__ __CUBE_PROXY_ADMIN_LISTEN__ __CUBE_PROXY_SSL_CERT__ __CUBE_PROXY_SSL_KEY__; do
			if ! grep -q -F "${token}" "${dst}"; then
				rm -f "${dst}"
				echo -e "  ${RED}✗ cube-proxy nginx template is missing ${token}; upstream CubeProxy/nginx.conf may have changed${NC}" >&2
				return 1
			fi
		done
		echo -e "  ${GREEN}✓ Generated cubeproxy-nginx.conf from ${src}${NC}"
		return 0
	fi

	cat >"${dst}" <<'EOF'
user root;
worker_processes auto;
error_log /data/log/cube-proxy/error.log notice;
daemon off;
events { worker_connections 100000; }
http {
  include mime.types;
  default_type application/octet-stream;
  server {
    listen __CUBE_PROXY_HTTP_PORT__;
    server_name _;
    location / { return 404; }
  }
  server {
    listen __CUBE_PROXY_HTTPS_PORT__ ssl;
    server_name _;
    ssl_certificate /usr/local/openresty/nginx/certs/__CUBE_PROXY_SSL_CERT__;
    ssl_certificate_key /usr/local/openresty/nginx/certs/__CUBE_PROXY_SSL_KEY__;
    location / { return 404; }
  }
  server {
    listen __CUBE_PROXY_ADMIN_LISTEN__:8082;
    server_name _;
    location / { return 404; }
  }
}
EOF
	echo -e "  ${YELLOW}⚠ cube-proxy nginx source not found; generated a minimal fallback that only satisfies probes and will not proxy sandbox traffic${NC}" >&2
	return 0
}

# cube-proxy's nginx.conf hard-codes these two TLS files; the cubeproxy-certs
# Secret (tke-addons.tf — a Secret, not a ConfigMap, because it holds the TLS
# private key) and cube-proxy's volume mounts are built from whatever sits in
# cubeproxy-certs/. Both files MUST exist before the addons are deployed, or
# terraform leaves out the Secret and cube-proxy CrashLoops on a missing cert.
CUBEPROXY_CERT_NAME="cube.app+3.pem"
CUBEPROXY_KEY_NAME="cube.app+3-key.pem"

# _require_cubeproxy_certs — fail fast unless BOTH required cube-proxy TLS files
#   are present locally (the directory terraform reads for the Secret). Call
#   this right before deploying the addons so a missing cert is a clear, early
#   error instead of a later Pod CrashLoop / absent Secret.
_require_cubeproxy_certs() {
	local cert_dir="${SCRIPT_DIR}/cubeproxy-certs"
	if [ -f "${cert_dir}/${CUBEPROXY_CERT_NAME}" ] && [ -f "${cert_dir}/${CUBEPROXY_KEY_NAME}" ]; then
		return 0
	fi
	echo -e "${RED}✗ cube-proxy TLS certificates are missing in ${cert_dir}${NC}" >&2
	echo -e "  ${YELLOW}cube-proxy requires both ${CUBEPROXY_CERT_NAME} and ${CUBEPROXY_KEY_NAME}.${NC}" >&2
	echo -e "  ${YELLOW}Fix it one of two ways, then re-run create.sh:${NC}" >&2
	echo -e "  ${YELLOW}  • let the deployer generate them (jumpserver + a resolvable bundle), or${NC}" >&2
	echo -e "  ${YELLOW}  • bring your own: drop the two files into ${cert_dir}/ yourself.${NC}" >&2
	return 1
}

# ---------------------------------------------------------------
# prepare_cubeproxy_certs — prepare TLS certificates for cube-proxy on TKE
#   In cube-proxy's nginx.conf, `listen 8080 ssl` hard-codes a reference to
#   /usr/local/openresty/nginx/certs/cube.app+3.pem(-key); this certificate is mounted via
#   the cubeproxy-certs Secret in tke-addons.tf. If the cubeproxy-certs/ directory
#   is missing, fileset() silently returns an empty set, and the cube-proxy container will CrashLoop because it cannot find the certificate.
#
#   Bring-your-own (BYO) certificate: if both cube.app+3.pem and
#   cube.app+3-key.pem already exist in cubeproxy-certs/, they are reused as-is
#   and no jumpserver generation happens. Otherwise the deployer generates them.
#
#   Certificate generation follows the mkcert approach of deploy/one-click/scripts/one-click/up-cube-proxy.sh:
#   on the jumpserver, use the bundled
#   assets/package/sandbox-package/support/bin/mkcert to run
#   `mkcert cube.app "*.cube.app" localhost 127.0.0.1` (generating cube.app+3.pem),
#   then send the certificate back to the local cubeproxy-certs/ where terraform runs (for the Secret to use),
#   while also keeping it on the jumpserver (/root/cubeproxy-certs).
#   Dependencies: jumpserver SSH(443) ready + sandbox-package already extracted on the jumpserver,
#   so it must be called after Phase 1.
#
#   Returns non-zero when it cannot produce both files AND no BYO certs exist, so
#   the caller can fail fast before deploying the addons.
# ---------------------------------------------------------------
prepare_cubeproxy_certs() {
	local cert_dir="${SCRIPT_DIR}/cubeproxy-certs"
	local cert_name="${CUBEPROXY_CERT_NAME}"
	local key_name="${CUBEPROXY_KEY_NAME}"
	local cert_file="${cert_dir}/${cert_name}"
	local key_file="${cert_dir}/${key_name}"

	# Reuse if it already exists locally to avoid re-signing each time. This is
	# also the BYO path: an operator-supplied cert/key pair is taken as-is.
	if [ -f "${cert_file}" ] && [ -f "${key_file}" ]; then
		echo -e "${GREEN}✓ cube-proxy TLS certificates are ready: ${cert_dir}${NC}"
		return 0
	fi

	local js_pub_ip ssh_key
	js_pub_ip=$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "")
	ssh_key="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"
	if [ -z "$js_pub_ip" ]; then
		# No jumpserver to generate on AND no BYO certs present (checked above):
		# fail rather than silently leaving cube-proxy without a certificate.
		echo -e "${RED}✗ jumpserver unavailable and no cube-proxy certificates present in ${cert_dir}${NC}" >&2
		echo -e "  ${YELLOW}Either complete the jumpserver provisioning, or drop ${cert_name} +${NC}" >&2
		echo -e "  ${YELLOW}${key_name} into ${cert_dir}/ yourself (BYO), then re-run.${NC}" >&2
		return 1
	fi

	# Ensure sandbox-package is already extracted on the jumpserver (contains support/bin/mkcert)
	local pkg_root
	pkg_root=$(_ensure_js_package) || true
	if [ -z "$pkg_root" ]; then
		echo -e "${RED}✗ Unable to prepare sandbox-package on the jumpserver (needed for cube-proxy cert generation)${NC}" >&2
		echo -e "  ${YELLOW}Set TENCENTCLOUD_LOCAL_BUNDLE to a resolvable bundle and retry, or drop${NC}" >&2
		echo -e "  ${YELLOW}${cert_name} + ${key_name} into ${cert_dir}/ yourself (BYO), then re-run.${NC}" >&2
		return 1
	fi

	local js_ssh=(
		ssh -i "${ssh_key}" -p 443
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
		-o ConnectTimeout=15 -o BatchMode=yes -o LogLevel=ERROR
	)
	local remote_cert_dir="/root/cubeproxy-certs"

	echo -e "  ${CYAN}Generating cube-proxy TLS certificates on the jumpserver with bundled mkcert...${NC}"
	# install_mkcert equivalent logic (see up-cube-proxy.sh): prefer the system mkcert,
	# otherwise install the bundled support/bin/mkcert, then sign with the same SAN list as the single-machine setup.
	local gen_out
	gen_out=$("${js_ssh[@]}" root@"${js_pub_ip}" "
		set -e
		MKCERT_BUNDLED='${pkg_root}/support/bin/mkcert'
		if ! command -v mkcert >/dev/null 2>&1; then
			if [ -x \"\${MKCERT_BUNDLED}\" ]; then
				install -m 0755 \"\${MKCERT_BUNDLED}\" /usr/local/bin/mkcert
			else
				echo 'MKCERT_NOT_FOUND'; exit 1
			fi
		fi
		mkdir -p '${remote_cert_dir}'
		cd '${remote_cert_dir}'
		if [ ! -f '${cert_name}' ] || [ ! -f '${key_name}' ]; then
			# -install only writes the local CA into the trust store (failure does not affect signing); signing itself creates the CA on demand
			mkcert -install >/dev/null 2>&1 || true
			mkcert cube.app '*.cube.app' localhost 127.0.0.1 >/dev/null 2>&1
		fi
		[ -f '${cert_name}' ] && [ -f '${key_name}' ] && echo 'CERT_OK' || echo 'CERT_MISSING'
	" 2>&1) || true

	if ! echo "$gen_out" | grep -q 'CERT_OK'; then
		echo -e "  ${RED}✗ jumpserver failed to generate cube-proxy certificate:${NC}"
		echo "$gen_out" | sed 's/^/    /'
		return 1
	fi

	# Send back to the local cubeproxy-certs/ where terraform runs (for the Secret in tke-addons.tf)
	mkdir -p "${cert_dir}"
	echo -e "  ${CYAN}Downloading certificates from the jumpserver to local: ${cert_dir}${NC}"
	if scp -i "${ssh_key}" -P 443 \
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=15 \
		"root@${js_pub_ip}:${remote_cert_dir}/${cert_name}" "${cert_file}" 1>&2 &&
		scp -i "${ssh_key}" -P 443 \
			-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=15 \
			"root@${js_pub_ip}:${remote_cert_dir}/${key_name}" "${key_file}" 1>&2; then
		chmod 600 "${key_file}" 2>/dev/null || true
		echo -e "  ${GREEN}✓ cube-proxy TLS certificates ready (jumpserver:${remote_cert_dir} + local:${cert_dir})${NC}"
	else
		echo -e "  ${RED}✗ Failed to download certificates from the jumpserver${NC}"
		rm -f "${cert_file}" "${key_file}"
		return 1
	fi
}

# ---------------------------------------------------------------
# Environment variable → Terraform variable mapping
# ---------------------------------------------------------------
setup_env() {
	TENCENTCLOUD_REGION="${TENCENTCLOUD_REGION:-ap-guangzhou}"
	export TF_VAR_region="$TENCENTCLOUD_REGION"
	[ -n "${TENCENTCLOUD_AVAILABILITY_ZONE:-}" ] && export TF_VAR_availability_zone="$TENCENTCLOUD_AVAILABILITY_ZONE"
	[ -n "${TENCENTCLOUD_JUMPSERVER_AVAILABILITY_ZONE:-}" ] && export TF_VAR_jumpserver_availability_zone="$TENCENTCLOUD_JUMPSERVER_AVAILABILITY_ZONE"
	[ -n "${TENCENTCLOUD_COMPUTE_AVAILABILITY_ZONE:-}" ] && export TF_VAR_compute_availability_zone="$TENCENTCLOUD_COMPUTE_AVAILABILITY_ZONE"
	[ -n "${TENCENTCLOUD_TKE_WORKER_AVAILABILITY_ZONE:-}" ] && export TF_VAR_tke_worker_availability_zone="$TENCENTCLOUD_TKE_WORKER_AVAILABILITY_ZONE"
	export TF_VAR_image_name_regex="${TENCENTCLOUD_IMAGE_NAME:-OpenCloudOS Server 9}"
	export TF_VAR_jumpserver_instance_type="${TENCENTCLOUD_JUMPSERVER_INSTANCE_TYPE:-SA9.MEDIUM4}"
	export TF_VAR_compute_instance_type="${TENCENTCLOUD_COMPUTE_INSTANCE_TYPE:-SA9.MEDIUM8}"
	[ -n "${TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE:-}" ] && export TF_VAR_compute_data_disk_size="$TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE"
	export TF_VAR_tke_worker_instance_type="${TENCENTCLOUD_TKE_WORKER_INSTANCE_TYPE:-SA9.LARGE8}"
	[ -n "${TENCENTCLOUD_VPC_NAME:-}" ] && export TF_VAR_vpc_name="$TENCENTCLOUD_VPC_NAME"

	SSH_PUB_KEY="${TENCENTCLOUD_SSH_PUBLIC_KEY_PATH:-$SCRIPT_DIR/.ssh/id_rsa.pub}"
	SSH_PRI_KEY="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SCRIPT_DIR/.ssh/id_rsa}"
	PVM_KERNEL_VMLINUX="${TENCENTCLOUD_PVM_KERNEL_VMLINUX:-$HOME/Downloads/vmlinux-pvm}"
	VERBOSE="${TENCENTCLOUD_VERBOSE:-1}"
	COMPUTE_NODE_COUNT="${TENCENTCLOUD_COMPUTE_NODE_COUNT:-}"
	TKE_CLUSTER_VERSION="${TENCENTCLOUD_TKE_CLUSTER_VERSION:-1.34.1}"
	TKE_NODE_COUNT="${TENCENTCLOUD_TKE_NODE_COUNT:-2}"
	CUBE_DB="${TENCENTCLOUD_CUBE_DB:-cube_mvp}"
	CUBE_USER="${TENCENTCLOUD_CUBE_USER:-cube}"
	CUBE_PASSWORD="${TENCENTCLOUD_CUBE_PASSWORD:-cube_pass}"
	CUBELET_NODE_STATUS_UPDATE_FREQUENCY="${TENCENTCLOUD_CUBELET_NODE_STATUS_UPDATE_FREQUENCY:-1s}"
	# Wire the cube DB name/user/password into Terraform so the MySQL account +
	# database (main.tf), the cube-master conf Secret (tke-addons.tf) and the health
	# checks below all use the SAME values. Without this the control plane would
	# keep the hard-coded defaults while the operator thinks they customized them
	# (silent drift / auth break).
	export TF_VAR_cube_db="$CUBE_DB"
	export TF_VAR_cube_user="$CUBE_USER"
	export TF_VAR_cube_password="$CUBE_PASSWORD"
	export TF_VAR_cubelet_node_status_update_frequency="$CUBELET_NODE_STATUS_UPDATE_FREQUENCY"
	# Resolve the deployment bundle: explicit TENCENTCLOUD_LOCAL_BUNDLE wins,
	# otherwise the user is asked interactively (local file path or web URL), with
	# auto-detection inside an extracted release bundle as the default. An empty
	# LOCAL_BUNDLE falls back to online download mode (requires public network).
	LOCAL_BUNDLE=""
	select_bundle
	REINSTALL="${TENCENTCLOUD_REINSTALL:-0}"
	RESET_DB="${TENCENTCLOUD_RESET_DB:-0}"
	# Default mode uses public pinned images and skips TCR build/push. Set
	# TENCENTCLOUD_USE_TCR=true to create/use a private TCR and build images.
	TENCENTCLOUD_USE_TCR="${TENCENTCLOUD_USE_TCR:-false}"
	TENCENTCLOUD_USE_CFS="${TENCENTCLOUD_USE_CFS:-false}"
	export TF_VAR_use_tcr="$TENCENTCLOUD_USE_TCR"
	export TF_VAR_use_cfs="$TENCENTCLOUD_USE_CFS"
	CUBE_IMAGE_TAG="${TENCENTCLOUD_CUBE_IMAGE_TAG:-v0.5.1}"
	export TF_VAR_image_tag="$CUBE_IMAGE_TAG"
	export TF_VAR_image_registry="${TENCENTCLOUD_IMAGE_REGISTRY:-cube-sandbox-cn.tencentcloudcr.com}"
	export TF_VAR_image_namespace="${TENCENTCLOUD_IMAGE_NAMESPACE:-cube-sandbox}"
	export TF_VAR_cubemaster_image="${TENCENTCLOUD_CUBEMASTER_IMAGE:-cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/cube-master:${CUBE_IMAGE_TAG}}"
	export TF_VAR_cubeapi_image="${TENCENTCLOUD_CUBEAPI_IMAGE:-cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/cube-api:${CUBE_IMAGE_TAG}}"
	export TF_VAR_cubeproxy_image="${TENCENTCLOUD_CUBEPROXY_IMAGE:-cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/cube-proxy:${CUBE_IMAGE_TAG}}"
	export TF_VAR_webui_image="${TENCENTCLOUD_WEBUI_IMAGE:-cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/webui:${CUBE_IMAGE_TAG}}"
	export TF_VAR_cube_lifecycle_manager_image="${TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_IMAGE:-cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/cube-lifecycle-manager:${CUBE_IMAGE_TAG}}"
	SSH_PORT="${TENCENTCLOUD_SSH_PORT:-22}"
	[ -n "${TENCENTCLOUD_MYSQL_PASSWORD:-}" ] && export TF_VAR_mysql_root_password="$TENCENTCLOUD_MYSQL_PASSWORD"
	[ -n "${TENCENTCLOUD_REDIS_PASSWORD:-}" ] && export TF_VAR_redis_password="$TENCENTCLOUD_REDIS_PASSWORD"
	export TF_VAR_tke_cluster_version="${TENCENTCLOUD_TKE_CLUSTER_VERSION:-1.34.1}"
	export TF_VAR_tke_node_count="$TKE_NODE_COUNT"
	export TF_VAR_cubemaster_replicas="${TENCENTCLOUD_CUBEMASTER_REPLICAS:-1}"
	export TF_VAR_cube_api_replicas="${TENCENTCLOUD_CUBE_API_REPLICAS:-1}"
	export TF_VAR_cube_proxy_replicas="${TENCENTCLOUD_CUBE_PROXY_REPLICAS:-1}"
	export TF_VAR_cube_lifecycle_manager_replicas="${TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_REPLICAS:-1}"
	export TF_VAR_cube_webui_replicas="${TENCENTCLOUD_CUBE_WEBUI_REPLICAS:-1}"
	export TF_VAR_cube_lifecycle_manager_default_idle_timeout="${TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_DEFAULT_IDLE_TIMEOUT:-5m}"
	export TF_VAR_cube_lifecycle_manager_heartbeat_ttl="${TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_HEARTBEAT_TTL:-15s}"
	export TF_VAR_cube_lifecycle_manager_discovery_refresh="${TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_DISCOVERY_REFRESH:-3s}"
	export TF_VAR_cube_admin_token="${TENCENTCLOUD_CUBE_ADMIN_TOKEN:-}"
	export TF_VAR_cube_proxy_heartbeat_interval_ms="${TENCENTCLOUD_CUBE_PROXY_HEARTBEAT_INTERVAL_MS:-5000}"
	# Network exposure mode. Default (false) fronts cube-api/cube-proxy/cube-webui
	# with VPC-internal CLBs; set to 'true' for public CLBs reachable from the
	# internet (see the "Hardening the Public-Facing Services" doc section).
	export TF_VAR_enable_public_network="${TENCENTCLOUD_ENABLE_PUBLIC_NETWORK:-false}"
	export TF_VAR_ssh_public_key_path="$SSH_PUB_KEY"
	export TF_VAR_ssh_private_key_path="$SSH_PRI_KEY"

	# If the SSH key does not exist, generate it automatically
	if [ ! -f "$SSH_PUB_KEY" ] || [ ! -f "$SSH_PRI_KEY" ]; then
		echo -e "  ${YELLOW}CubeSandbox SSH key does not exist, generating automatically...${NC}"
		local _key_dir
		_key_dir="$(dirname "$SSH_PUB_KEY")"
		mkdir -p "$_key_dir"
		# Clean up leftover directories (in case there is a directory with the same name instead of a file)
		[ -d "$SSH_PRI_KEY" ] && rm -rf "$SSH_PRI_KEY"
		[ -d "$SSH_PUB_KEY" ] && rm -rf "$SSH_PUB_KEY"
		ssh-keygen -t rsa -b 4096 -f "$_key_dir/id_rsa" -N "" -q 2>&1 || {
			echo -e "${RED}✗ SSH key generation failed${NC}"
			echo ""
			echo "  Or specify another key via environment variables:"
			echo "    export TENCENTCLOUD_SSH_PUBLIC_KEY_PATH=/path/to/key.pub"
			echo "    export TENCENTCLOUD_SSH_PRIVATE_KEY_PATH=/path/to/key"
			echo ""
			exit 1
		}
		chmod 600 "$SSH_PRI_KEY"
		chmod 644 "$SSH_PUB_KEY"
		echo -e "  ${GREEN}✓ SSH key generated: ${SSH_PUB_KEY}${NC}"
	fi
	echo -e "${GREEN}✓ SSH public key: ${SSH_PUB_KEY}${NC}"
}

# _js_pub_ip — the jumpserver public IP, cached after the first successful
# lookup. `terraform output` parses the whole state, and the SSH helpers below
# run inside polling loops (health checks, HTTP probes, node verification), so
# re-querying it on every call is wasteful. Empty results are NOT cached, so it
# self-heals if the jumpserver does not exist yet.
JS_PUB_IP_CACHE=""
_js_pub_ip() {
	if [ -z "${JS_PUB_IP_CACHE:-}" ]; then
		JS_PUB_IP_CACHE=$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "")
	fi
	printf '%s' "${JS_PUB_IP_CACHE}"
}

# Build SSH options that proxy through the jumpserver
# Usage: _setup_jump_proxy — sets the JUMP_PROXY_OPTS global array
_setup_jump_proxy() {
	local js_pub_ip key_file
	js_pub_ip=$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "")
	key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"
	if [ -z "$js_pub_ip" ]; then
		JUMP_PROXY_OPTS=()
	else
		# Quote the key path inside the ProxyCommand (it is re-parsed by /bin/sh)
		# so a user-supplied TENCENTCLOUD_SSH_PRIVATE_KEY_PATH containing spaces
		# does not break the proxy hop.
		JUMP_PROXY_OPTS=(-o "ProxyCommand=ssh -i '${key_file}' -p 443 -W %h:%p -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o BatchMode=yes -o LogLevel=ERROR root@${js_pub_ip}")
	fi
}

# Upload the SSH private key and kubeconfig to the jumpserver
_setup_jumpserver_key() {
	local js_pub_ip key_file
	js_pub_ip=$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "")
	key_file="${SSH_PRI_KEY}"
	if [ -z "$js_pub_ip" ] || [ ! -f "$key_file" ]; then
		return 1
	fi
	echo -e "  ${CYAN}Uploading the SSH private key to the jumpserver...${NC}"
	scp -i "${key_file}" -P 443 \
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o ConnectTimeout=10 \
		"${key_file}" "root@${js_pub_ip}:/root/.ssh/id_rsa" 2>&1 || {
		echo -e "  ${RED}✗ Private key upload failed${NC}"
		return 1
	}
	ssh -i "${key_file}" -p 443 \
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o ConnectTimeout=10 root@"${js_pub_ip}" \
		"mkdir -p /root/.ssh /root/.kube && chmod 700 /root/.ssh && chmod 600 /root/.ssh/id_rsa" 2>&1 || true
	echo -e "  ${GREEN}✓ jumpserver can now reach the internal nodes${NC}"

	# Upload kubeconfig
	if terraform output -raw tke_kube_config 2>/dev/null | grep -q '^apiVersion'; then
		echo -e "  ${CYAN}Uploading kubeconfig to the jumpserver...${NC}"
		terraform output -raw tke_kube_config 2>/dev/null |
			ssh -i "${key_file}" -p 443 \
				-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
				-o ConnectTimeout=10 root@"${js_pub_ip}" \
				"cat > /root/.kube/config && chmod 600 /root/.kube/config" 2>&1 || true
		echo -e "  ${GREEN}✓ kubeconfig uploaded${NC}"
	fi
}

# Execute kubectl commands on the jumpserver
_js_kubectl() {
	local js_pub_ip key_file
	js_pub_ip=$(_js_pub_ip)
	key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"
	if [ -z "$js_pub_ip" ]; then
		echo "" && return 1
	fi
	ssh -i "${key_file}" -p 443 \
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o ConnectTimeout=5 -o BatchMode=yes -o LogLevel=ERROR \
		root@"${js_pub_ip}" "kubectl $*" 2>&1 || true
}

# Install cubemastercli on the jumpserver (extracted from the bundle)
_install_cubemastercli() {
	local js_pub_ip key_file
	js_pub_ip=$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "")
	key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"
	[ -z "$js_pub_ip" ] && return 1

	local js_ssh=(
		ssh -i "${key_file}" -p 443
		-o StrictHostKeyChecking=no
		-o UserKnownHostsFile=/dev/null
		-o ConnectTimeout=10
		-o BatchMode=yes
		-o LogLevel=ERROR
	)

	# Check whether it is already installed
	local already
	already=$("${js_ssh[@]}" root@"${js_pub_ip}" "command -v cubemastercli 2>&1" 2>&1) || true
	if echo "$already" | grep -q "cubemastercli"; then
		echo -e "  ${GREEN}✓ cubemastercli already exists (${already})${NC}"
		return 0
	fi

	echo -e "  ${CYAN}Installing cubemastercli on the jumpserver...${NC}"

	if [ -n "${LOCAL_BUNDLE:-}" ]; then
		# Local bundle: verify → upload → extract cubemastercli
		local bundle_name
		bundle_name="$(basename "${LOCAL_BUNDLE}")"

		# Compute the local md5
		local local_md5
		local_md5=$(md5sum "${LOCAL_BUNDLE}" 2>/dev/null | awk '{print $1}' || echo "")
		if [ -z "$local_md5" ] && command -v md5 &>/dev/null; then
			local_md5=$(md5 -q "${LOCAL_BUNDLE}" 2>/dev/null || echo "")
		fi

		# Check the bundle md5 on the jumpserver
		local remote_md5 need_upload=0
		remote_md5=$("${js_ssh[@]}" root@"${js_pub_ip}" "
      if [ -f /tmp/${bundle_name} ]; then
        md5sum /tmp/${bundle_name} 2>/dev/null | awk '{print \$1}' || md5 -q /tmp/${bundle_name} 2>/dev/null || echo 'NO_MD5'
      else
        echo 'NOT_FOUND'
      fi
    " 2>&1) || true
		remote_md5=$(echo "$remote_md5" | tr -d '\r\n ')

		if [ "$remote_md5" = "NOT_FOUND" ]; then
			echo -e "  ${YELLOW}No bundle on the jumpserver, upload required${NC}"
			need_upload=1
		elif [ -n "$local_md5" ] && [ -n "$remote_md5" ] && [ "$local_md5" != "$remote_md5" ] && [ "$remote_md5" != "NO_MD5" ]; then
			echo -e "  ${YELLOW}md5 mismatch (local: ${local_md5}, remote: ${remote_md5}), re-uploading${NC}"
			need_upload=1
		else
			echo -e "  ${GREEN}✓ bundle md5 matches (${local_md5}), skipping upload${NC}"
		fi

		if [ "$need_upload" -eq 1 ]; then
			echo -e "  ${CYAN}Uploading the bundle to the jumpserver...${NC}"
			scp -i "${key_file}" -P 443 \
				-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
				-o ConnectTimeout=10 \
				"${LOCAL_BUNDLE}" "root@${js_pub_ip}:/tmp/${bundle_name}" 2>&1 || {
				echo -e "  ${YELLOW}⚠ bundle upload failed, skipping cubemastercli installation${NC}"
				return 1
			}
			BUNDLE_UPDATED=1 # Mark the bundle as updated; compute nodes need redistribution
		fi

		"${js_ssh[@]}" root@"${js_pub_ip}" "
      set -e
      mkdir -p /tmp/cube-bundle /tmp/cube-package
      # 1) Extract the outer bundle
      tar -xzf /tmp/${bundle_name} -C /tmp/cube-bundle/
      BUNDLE_DIR=\$(ls -d /tmp/cube-bundle/*/ 2>/dev/null | head -1)
      # 2) Extract assets/package/sandbox-package.tar.gz
      PKG_TAR=\"\${BUNDLE_DIR}/assets/package/sandbox-package.tar.gz\"
      if [ -f \"\${PKG_TAR}\" ]; then
        tar -xzf \"\${PKG_TAR}\" -C /tmp/cube-package/
        # 3) Find cubemastercli and install it
        CLI_BIN=\$(find /tmp/cube-package -name cubemastercli -type f 2>/dev/null | head -1)
        if [ -n \"\${CLI_BIN}\" ]; then
          cp \${CLI_BIN} /usr/local/bin/cubemastercli
          chmod +x /usr/local/bin/cubemastercli
          echo 'INSTALLED'
        else
          echo 'NOT_FOUND: cubemastercli not in sandbox-package'
        fi
      else
        echo \"NOT_FOUND: \${PKG_TAR} not found\"
      fi
    " 2>&1 || echo -e "  ${YELLOW}⚠ cubemastercli extraction failed${NC}"
	else
		# Online mode: download and extract on the jumpserver
		local cn_url="https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh"
		local gh_url="https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh"

		"${js_ssh[@]}" root@"${js_pub_ip}" "
      # Download the online install script, extract the bundle download URL
      ONLINE_SCRIPT=\$(curl -fsSL --connect-timeout 10 --max-time 30 '${cn_url}' 2>/dev/null || \\
                       curl -fsSL --connect-timeout 10 --max-time 30 '${gh_url}' 2>/dev/null)
      BUNDLE_URL=\$(echo \"\$ONLINE_SCRIPT\" | grep -oE 'https://[^ ]*cube-sandbox-one-click[^ ]*\.tar\.gz' | head -1)
      if [ -z \"\$BUNDLE_URL\" ]; then
        echo 'SKIP: cannot determine bundle URL'
        exit 0
      fi
      echo \"Downloading: \$BUNDLE_URL\"
      mkdir -p /tmp/cube-bundle /tmp/cube-package
      cd /tmp/cube-bundle
      curl -fsSL --connect-timeout 10 --max-time 120 \"\$BUNDLE_URL\" -o bundle.tar.gz
      tar -xzf bundle.tar.gz
      BUNDLE_DIR=\$(ls -d */ 2>/dev/null | head -1)
      PKG_TAR=\"\${BUNDLE_DIR}/assets/package/sandbox-package.tar.gz\"
      if [ -f \"\${PKG_TAR}\" ]; then
        tar -xzf \"\${PKG_TAR}\" -C /tmp/cube-package/
        CLI_BIN=\$(find /tmp/cube-package -name cubemastercli -type f 2>/dev/null | head -1)
        if [ -n \"\${CLI_BIN}\" ]; then
          cp \${CLI_BIN} /usr/local/bin/cubemastercli
          chmod +x /usr/local/bin/cubemastercli
          echo 'INSTALLED'
        else
          echo 'NOT_FOUND'
        fi
      else
        echo 'NOT_FOUND'
      fi
    " 2>&1 || echo -e "  ${YELLOW}⚠ cubemastercli online installation failed${NC}"
	fi

	# Verify
	local verify
	verify=$("${js_ssh[@]}" root@"${js_pub_ip}" "cubemastercli --help 2>&1 | head -1" 2>&1) || true
	if [ -n "$verify" ]; then
		echo -e "  ${GREEN}✓ cubemastercli installed (jumpserver:/usr/local/bin/cubemastercli)${NC}"
	fi
}

# ---------------------------------------------------------------
# ensure_js_bundle — on EVERY run, make sure the deployment bundle tar.gz exists
#   on the jumpserver (/tmp/<bundle>) and its md5 matches the local bundle;
#   (re)upload when it is missing or differs. Runs regardless of whether
#   cubemastercli is already installed (unlike _install_cubemastercli, which
#   short-circuits), so a changed local bundle is always re-synced before the
#   build/extract steps consume it. Sets BUNDLE_UPDATED=1 when a fresh copy was
#   uploaded so the compute nodes get the new bundle redistributed. No-op in
#   online mode (no local bundle to compare against).
# ---------------------------------------------------------------
ensure_js_bundle() {
	[ -n "${LOCAL_BUNDLE:-}" ] || return 0

	local js_pub_ip key_file bundle_name local_md5 remote_md5
	js_pub_ip=$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "")
	key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"
	[ -z "$js_pub_ip" ] && return 0
	bundle_name="$(basename "${LOCAL_BUNDLE}")"

	local js_ssh=(
		ssh -i "${key_file}" -p 443
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
		-o ConnectTimeout=10 -o BatchMode=yes -o LogLevel=ERROR
	)

	echo -e "  ${CYAN}Checking the deployment bundle on the jumpserver (/tmp/${bundle_name})...${NC}"

	local_md5=$(md5sum "${LOCAL_BUNDLE}" 2>/dev/null | awk '{print $1}' || echo "")
	if [ -z "$local_md5" ] && command -v md5 >/dev/null 2>&1; then
		local_md5=$(md5 -q "${LOCAL_BUNDLE}" 2>/dev/null || echo "")
	fi

	remote_md5=$("${js_ssh[@]}" root@"${js_pub_ip}" "
    if [ -f /tmp/${bundle_name} ]; then
      md5sum /tmp/${bundle_name} 2>/dev/null | awk '{print \$1}' || echo 'NO_MD5'
    else
      echo 'NOT_FOUND'
    fi
  " 2>/dev/null | tr -d '\r\n ')

	if [ "$remote_md5" = "NOT_FOUND" ] || [ -z "$remote_md5" ]; then
		echo -e "  ${YELLOW}Bundle not present on the jumpserver; uploading...${NC}"
	elif [ -n "$local_md5" ] && [ "$local_md5" != "$remote_md5" ] && [ "$remote_md5" != "NO_MD5" ]; then
		echo -e "  ${YELLOW}Bundle md5 mismatch (local: ${local_md5}, remote: ${remote_md5}); re-uploading...${NC}"
	else
		echo -e "  ${GREEN}✓ Bundle present and md5 matches (${local_md5:-unknown})${NC}"
		return 0
	fi

	scp -i "${key_file}" -P 443 \
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o ConnectTimeout=10 \
		"${LOCAL_BUNDLE}" "root@${js_pub_ip}:/tmp/${bundle_name}" 1>&2 || {
		echo -e "  ${RED}✗ Bundle upload failed${NC}"
		return 1
	}
	BUNDLE_UPDATED=1
	echo -e "  ${GREEN}✓ Bundle uploaded (${local_md5:-unknown})${NC}"
}

# ---------------------------------------------------------------
# _ensure_js_package — ensure sandbox-package is already extracted on the jumpserver
#   sandbox-package contains build_images.sh and the Dockerfiles + precompiled artifacts for the four components.
#   On success, print the remote package root directory (/tmp/cube-package/sandbox-package) to stdout;
#   on failure, print an empty string and return 1.
# ---------------------------------------------------------------
_ensure_js_package() {
	local js_pub_ip key_file
	js_pub_ip=$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "")
	key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"
	[ -z "$js_pub_ip" ] && {
		echo ""
		return 1
	}

	local js_ssh=(
		ssh -i "${key_file}" -p 443
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
		-o ConnectTimeout=10 -o BatchMode=yes -o LogLevel=ERROR
	)

	local pkg_root="/tmp/cube-package/sandbox-package"
	local build_script="${pkg_root}/terraform/tencentcloud/build_images.sh"

	if [ -n "${LOCAL_BUNDLE:-}" ]; then
		# Local bundle: re-upload when the jumpserver copy is missing or differs from
		# the current local bundle (md5), then ALWAYS re-extract sandbox-package so
		# every run builds & pushes from the latest bundle (no stale-cache reuse).
		local bundle_name local_md5 remote_md5
		bundle_name="$(basename "${LOCAL_BUNDLE}")"
		local_md5=$(md5sum "${LOCAL_BUNDLE}" 2>/dev/null | awk '{print $1}' || echo "")
		if [ -z "$local_md5" ] && command -v md5 >/dev/null 2>&1; then
			local_md5=$(md5 -q "${LOCAL_BUNDLE}" 2>/dev/null || echo "")
		fi
		remote_md5=$("${js_ssh[@]}" root@"${js_pub_ip}" \
			"md5sum /tmp/${bundle_name} 2>/dev/null | awk '{print \$1}' || echo ''" 2>/dev/null | tr -d '\r\n ')
		if [ -z "$remote_md5" ] || { [ -n "$local_md5" ] && [ "$local_md5" != "$remote_md5" ]; }; then
			# Send scp's progress bar to stderr (the terminal); this function's
			# stdout is captured by the caller, so it must stay clean.
			scp -i "${key_file}" -P 443 \
				-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
				-o ConnectTimeout=10 \
				"${LOCAL_BUNDLE}" "root@${js_pub_ip}:/tmp/${bundle_name}" 1>&2 || {
				echo ""
				return 1
			}
		fi
		"${js_ssh[@]}" root@"${js_pub_ip}" "
      set -e
      rm -rf /tmp/cube-bundle /tmp/cube-package
      mkdir -p /tmp/cube-bundle /tmp/cube-package
      tar -xzf /tmp/${bundle_name} -C /tmp/cube-bundle/
      BUNDLE_DIR=\$(ls -d /tmp/cube-bundle/*/ 2>/dev/null | head -1)
      PKG_TAR=\"\${BUNDLE_DIR}/assets/package/sandbox-package.tar.gz\"
      [ -f \"\${PKG_TAR}\" ] && tar -xzf \"\${PKG_TAR}\" -C /tmp/cube-package/
    " >/dev/null 2>&1 || {
			echo ""
			return 1
		}
	else
		# Online mode: reuse the already-extracted package if present (avoid
		# re-downloading the bundle from the network on every run), otherwise
		# download the bundle and extract sandbox-package.
		if "${js_ssh[@]}" root@"${js_pub_ip}" "[ -f ${build_script} ]" 2>/dev/null; then
			echo "${pkg_root}"
			return 0
		fi
		local cn_url="https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh"
		local gh_url="https://github.com/tencentcloud/CubeSandbox/raw/master/deploy/one-click/online-install.sh"
		"${js_ssh[@]}" root@"${js_pub_ip}" "
      set -e
      ONLINE_SCRIPT=\$(curl -fsSL --connect-timeout 10 --max-time 30 '${cn_url}' 2>/dev/null || \\
                       curl -fsSL --connect-timeout 10 --max-time 30 '${gh_url}' 2>/dev/null)
      BUNDLE_URL=\$(echo \"\$ONLINE_SCRIPT\" | grep -oE 'https://[^ ]*cube-sandbox-one-click[^ ]*\.tar\.gz' | head -1)
      [ -z \"\$BUNDLE_URL\" ] && exit 1
      mkdir -p /tmp/cube-bundle /tmp/cube-package
      cd /tmp/cube-bundle
      curl -fsSL --connect-timeout 10 --max-time 300 \"\$BUNDLE_URL\" -o bundle.tar.gz
      tar -xzf bundle.tar.gz
      BUNDLE_DIR=\$(ls -d */ 2>/dev/null | head -1)
      PKG_TAR=\"\${BUNDLE_DIR}/assets/package/sandbox-package.tar.gz\"
      [ -f \"\${PKG_TAR}\" ] && tar -xzf \"\${PKG_TAR}\" -C /tmp/cube-package/
    " >/dev/null 2>&1 || {
			echo ""
			return 1
		}
	fi

	if "${js_ssh[@]}" root@"${js_pub_ip}" "[ -f ${build_script} ]" 2>/dev/null; then
		echo "${pkg_root}"
		return 0
	fi
	echo ""
	return 1
}

# ---------------------------------------------------------------
# _tcr_login_retry — log in to the TCR registry on the jumpserver (docker login),
#   retrying on failure. The TCR instance / access token / internal-network DNS
#   are created by terraform immediately before this runs and can take a short
#   while to become usable, so the first login attempt often fails transiently.
#   Retry up to 3 times, waiting 10s between attempts, instead of giving up at once.
#   Args: <js_pub_ip> <registry> <token_user> <js_ssh array...>
#   Echoes progress; returns 0 on success, 1 after 3 consecutive failures.
# ---------------------------------------------------------------
_tcr_login_retry() {
	local js_pub_ip="$1" reg="$2" user="$3"
	shift 3
	local js_ssh=("$@")
	local attempt max=3
	for attempt in $(seq 1 "$max"); do
		if "${js_ssh[@]}" root@"${js_pub_ip}" \
			"[ -f /root/.tcr_token ] && docker login ${reg} --username ${user} --password-stdin < /root/.tcr_token >/dev/null 2>&1"; then
			echo -e "  ${GREEN}✓ TCR login successful${NC}"
			return 0
		fi
		if [ "$attempt" -lt "$max" ]; then
			echo -e "  ${YELLOW}⚠ TCR login failed (attempt ${attempt}/${max}); retrying in 10s...${NC}"
			sleep 10
		fi
	done
	echo -e "  ${RED}✗ TCR login failed ${max} times in a row${NC}"
	return 1
}

# ---------------------------------------------------------------
# cache_base_images — pre-pull the base images that build_images.sh builds FROM
#   (e.g. alpine:3.21, ubuntu:24.04, rust:1.85-alpine) from the PUBLIC Tencent
#   registry mirror (cube-sandbox-image.tencentcloudcr.com/opensource/*) onto the
#   jumpserver and retag them to their canonical docker.io names. This MUST run
#   before build_and_push_images: the component Dockerfiles build FROM these
#   bases, and the jumpserver often cannot reach docker.io directly, so the build
#   would otherwise fail on missing base images. Only needed when docker.io is
#   unreachable; if it is reachable docker pulls the bases itself during the
#   build. NOTE: the mirror namespace is hard-coded and unrelated to the TCR this
#   run creates (we still log in to the run's TCR first only because the build
#   pushes there); the pulls are best-effort (a miss just defers to the build).
# ---------------------------------------------------------------
cache_base_images() {
	if [ "${TENCENTCLOUD_BUILD_IMAGES:-1}" = "0" ]; then
		return 0
	fi

	local js_pub_ip key_file reg user
	js_pub_ip=$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "")
	key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"
	reg=$(terraform output -raw tcr_registry_name 2>/dev/null || echo "")
	user=$(terraform output -raw tcr_token_user 2>/dev/null || echo "")
	if [ -z "$js_pub_ip" ] || [ -z "$reg" ]; then
		return 0
	fi

	local js_ssh=(
		ssh -i "${key_file}" -p 443
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
		-o ConnectTimeout=15 -o BatchMode=yes -o LogLevel=ERROR
	)

	echo -e "  ${CYAN}Logging in to TCR (jumpserver): ${reg}...${NC}"
	if ! _tcr_login_retry "$js_pub_ip" "$reg" "$user" "${js_ssh[@]}"; then
		echo -e "  ${RED}✗ Could not log in to TCR after 3 attempts; aborting.${NC}"
		echo -e "  ${YELLOW}  The TCR may not be ready yet. Please re-run create.sh to try again.${NC}"
		exit 1
	fi

	# When docker.io is reachable, docker pulls the base images itself; nothing to do.
	if "${js_ssh[@]}" root@"${js_pub_ip}" \
		"curl -s --connect-timeout 5 https://registry-1.docker.io/v2/ >/dev/null 2>&1"; then
		return 0
	fi

	echo -e "  ${YELLOW}⚠ docker.io unreachable, pulling base images from TCR...${NC}"
	local _img _src _dst
	for _img in \
		"cube-sandbox-image.tencentcloudcr.com/opensource/alpine:3.21>alpine:3.21" \
		"cube-sandbox-image.tencentcloudcr.com/opensource/ubuntu:24.04>ubuntu:24.04" \
		"cube-sandbox-image.tencentcloudcr.com/opensource/rust:1.85-alpine>rust:1.85-alpine"; do
		_src="${_img%%>*}" _dst="${_img##*>}"
		"${js_ssh[@]}" root@"${js_pub_ip}" \
			"docker pull ${_src} 2>&1 && docker tag ${_src} ${_dst} 2>&1" 2>&1 || true
	done
	echo -e "  ${GREEN}✓ base images cached${NC}"
	echo ""
}

# ---------------------------------------------------------------
# build_and_push_images — build the four component images on the jumpserver and push them to the TCR created this run
#   TKE addons then use these images directly (see local.image_registry in tke-addons.tf).
#   The jumpserver already has docker installed, holds the TCR token, and is in the same VPC as the TCR, making it a natural build machine.
#   build_images.sh is executed ON the jumpserver (over SSH) to build and push.
#   Can be skipped via TENCENTCLOUD_BUILD_IMAGES=0 (reuse existing images).
#   Returns 0 when images are available for the addons (built+pushed, or reuse),
#   and non-zero when the build/push could not complete — callers use this to
#   skip the TKE addons deployment and the follow-up health checks.
# ---------------------------------------------------------------
build_and_push_images() {
	if [ "${TENCENTCLOUD_BUILD_IMAGES:-1}" = "0" ]; then
		echo -e "  ${YELLOW}TENCENTCLOUD_BUILD_IMAGES=0, skipping image build (reuse existing TCR images)${NC}"
		return 0
	fi

	banner "Phase: Build component images and push to TCR"

	local js_pub_ip key_file reg ns user tag
	js_pub_ip=$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "")
	key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"
	reg=$(terraform output -raw tcr_registry_name 2>/dev/null || echo "")
	ns=$(terraform output -raw tcr_namespace 2>/dev/null || echo "")
	user=$(terraform output -raw tcr_token_user 2>/dev/null || echo "")
	tag="${CUBE_IMAGE_TAG:-v0.5.1}"

	if [ -z "$js_pub_ip" ] || [ -z "$reg" ] || [ -z "$ns" ]; then
		echo -e "  ${RED}✗ Missing jumpserver / TCR info; cannot build images${NC}"
		return 1
	fi
	echo -e "  ${CYAN}Build host (jumpserver): ${js_pub_ip}${NC}"
	echo -e "  ${CYAN}Target TCR            : ${reg}/${ns} (tag=${tag})${NC}"

	local js_ssh=(
		ssh -i "${key_file}" -p 443
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
		-o ConnectTimeout=15 -o BatchMode=yes -o LogLevel=ERROR
	)

	# 1) jumpserver needs docker (installed by cloud-init, with fault tolerance)
	if ! "${js_ssh[@]}" root@"${js_pub_ip}" "command -v docker >/dev/null 2>&1 && systemctl is-active docker >/dev/null 2>&1" 2>/dev/null; then
		echo -e "  ${RED}✗ docker unavailable on the jumpserver; cannot build images${NC}"
		return 1
	fi

	# 2) Ensure the build context (sandbox-package) is ready
	echo -e "  ${CYAN}Preparing build context (sandbox-package) on the jumpserver...${NC}"
	local pkg_root
	# Return non-zero on failure to avoid set -e exiting the script at the assignment
	pkg_root=$(_ensure_js_package) || true
	if [ -z "$pkg_root" ]; then
		echo -e "  ${RED}✗ Unable to prepare sandbox-package on the jumpserver; cannot build images${NC}"
		echo -e "  ${YELLOW}  (you can set TENCENTCLOUD_LOCAL_BUNDLE to point to a local bundle and retry)${NC}"
		return 1
	fi

	# 3) Log in to TCR (the token is written to jumpserver:/root/.tcr_token by terraform)
	echo -e "  ${CYAN}Logging in to TCR on the jumpserver: ${reg} ...${NC}"
	if ! _tcr_login_retry "$js_pub_ip" "$reg" "$user" "${js_ssh[@]}"; then
		echo -e "  ${RED}✗ Could not log in to TCR after 3 attempts; cannot push images.${NC}"
		echo -e "  ${YELLOW}  The TCR may not be ready yet. Please re-run create.sh to try again.${NC}"
		exit 1
	fi

	# 4) Build and push (REGISTRY/NAMESPACE/TAG match the defaults in tke-addons.tf)
	echo -e "  ${CYAN}Running build_images.sh on the jumpserver: building & pushing${NC}"
	echo -e "  ${CYAN}  cubemaster/cube-api/cubeproxy/cube-lifecycle-manager/cube-webui → ${reg}/${ns} (tag=${tag})...${NC}"
	echo -e "  ${YELLOW}  (docker build produces a lot of output, please be patient)${NC}"
	if "${js_ssh[@]}" root@"${js_pub_ip}" \
		"REGISTRY='${reg}' NAMESPACE='${ns}' TAG='${tag}' PUSH=1 bash '${pkg_root}/terraform/tencentcloud/build_images.sh' all"; then
		echo -e "  ${GREEN}✓ Images built and pushed to TCR (${reg}/${ns})${NC}"
		echo ""
		return 0
	fi

	echo -e "  ${RED}✗ Image build/push failed on the jumpserver${NC}"
	echo -e "  ${YELLOW}  You can log in to the jumpserver and run build_images.sh manually to troubleshoot${NC}"
	echo ""
	return 1
}

# ---------------------------------------------------------------
# tcr_build_and_push — STEP 2: build & push the component images to TCR. Runs
#   after STEP 1 has created the TCR + jumpserver (the jumpserver is up and can
#   log in to TCR) and before STEP 3 deploys the TKE addons (which pull these
#   images). It first confirms the TCR service is ready (which also proves this
#   is a TKE deployment), then (only when not env-configured) asks whether to
#   build now and which image tag to use, and finally runs the base-image cache +
#   build/push (which perform the jumpserver→TCR docker login internally). Sets
#   the global IMAGES_OK (0 on a build failure) so STEP 3 can skip the addons.
# ---------------------------------------------------------------
tcr_build_and_push() {
	banner "Step: Build component images and push to TCR"

	# Always run the TCR login + image build/push on EVERY create.sh run. This
	# executes right after STEP 1 verified the jumpserver SSH (443) and created the
	# TCR, so the build host and registry are ready. Only skip when there is
	# genuinely no TCR registry in state (e.g. a pure CVM deployment); the actual
	# TCR login + build + push happen in build_and_push_images below, which reports
	# any real failure and sets IMAGES_OK=0 (rather than silently skipping).
	local reg ns
	reg=$(terraform output -raw tcr_registry_name 2>/dev/null || echo "")
	ns=$(terraform output -raw tcr_namespace 2>/dev/null || echo "")
	if [ -z "$reg" ]; then
		echo -e "  ${YELLOW}No TCR registry found in state; skipping image build/push.${NC}"
		echo ""
		return 0
	fi
	echo -e "  ${GREEN}✓ TCR: ${reg}/${ns}${NC}"
	echo ""

	# TKE is always created, so the component images are always needed; mark the
	# terraform intent so the later addons deployment treats this as a TKE run.
	export TF_VAR_create_tke=true

	# Honor the explicit skip switch (reuse existing TCR images).
	if [ "${TENCENTCLOUD_BUILD_IMAGES:-1}" = "0" ]; then
		echo -e "  ${YELLOW}TENCENTCLOUD_BUILD_IMAGES=0, skipping image build (reuse existing TCR images)${NC}"
		echo ""
		IMAGES_OK=1
		return 0
	fi

	# Build & push always proceeds by default — no interactive prompt. The only
	# opt-out is TENCENTCLOUD_BUILD_IMAGES=0 (handled above, reuse existing TCR
	# images).
	if [ "${IMAGES_CONFIGURED:-0}" = "1" ]; then
		echo -e "  ${GREEN}✓ Build & push images: yes (configured via environment)${NC}"
	else
		echo -e "  ${GREEN}✓ Build & push images: yes (default)${NC}"
	fi

	# The image tag was already resolved earlier (env / saved selection /
	# prompt_deployment_env / default), so don't ask again — just remind which tag
	# will be built & pushed.
	echo -e "  ${GREEN}✓ Image tag to build & push: ${CUBE_IMAGE_TAG:-v0.5.1}${NC}"
	echo ""

	# Pre-pull the base images the build needs from the in-VPC TCR mirror first
	# (the docker build can fail when the jumpserver cannot reach docker.io), then
	# build & push. Both helpers do the jumpserver→TCR docker login themselves.
	cache_base_images
	IMAGES_OK=1
	build_and_push_images || IMAGES_OK=0
}

check_credentials() {
	if [ -z "${TENCENTCLOUD_SECRET_ID:-}" ] || [ -z "${TENCENTCLOUD_SECRET_KEY:-}" ]; then
		echo -e "${RED}Error: please set the Tencent Cloud API credentials first${NC}"
		echo ""
		echo "  export TENCENTCLOUD_SECRET_ID=\"your-secret-id\""
		echo "  export TENCENTCLOUD_SECRET_KEY=\"your-secret-key\""
		echo ""
		echo -e "  ${CYAN}Create an API key pair (SecretId / SecretKey) in the console:${NC}"
		echo -e "  ${CYAN}  https://console.cloud.tencent.com/cam/capi${NC}"
		echo -e "  ${CYAN}For the other supported variables, see ${SCRIPT_DIR}/env.example${NC}"
		echo ""
		exit 1
	fi
	echo -e "${GREEN}✓ Keys configured${NC}"
}

# ---------------------------------------------------------------
# confirm_env — when an env var was not explicitly set, let the user confirm
#   the default (press Enter) or type a custom value. The resolved value is
#   exported back under the same TENCENTCLOUD_* name so setup_env() consumes it
#   through its normal mapping. On a non-interactive shell (no TTY) the default
#   is accepted silently so CI / piped runs keep working.
#   Usage: confirm_env VAR_NAME "Human label" "default value" [secret]
# ---------------------------------------------------------------
confirm_env() {
	local var="$1" label="$2" default="$3" secret="${4:-}"

	# Already provided by the user → respect it, do not prompt.
	if [ -n "${!var:-}" ]; then
		if [ "$secret" = "secret" ]; then
			echo -e "  ${GREEN}✓ ${label} (from \$${var})${NC}"
		else
			echo -e "  ${GREEN}✓ ${label} (from \$${var}): ${!var}${NC}"
		fi
		return 0
	fi

	# No interactive terminal → accept the default silently.
	if [ ! -t 0 ]; then
		export "${var}=${default}"
		echo -e "  ${GREEN}✓ ${label}: ${default} ${CYAN}(default)${NC}"
		return 0
	fi

	local input
	read -r -p "$(echo -e "${YELLOW}${label} [${CYAN}${default}${YELLOW}]: ${NC}")" input
	[ -z "${input}" ] && input="${default}"
	export "${var}=${input}"
	if [ "$secret" = "secret" ]; then
		echo -e "  ${GREEN}✓ ${label} set${NC}"
	else
		echo -e "  ${GREEN}✓ ${label}: ${input}${NC}"
	fi
}

# ---------------------------------------------------------------
# select_env — like confirm_env, but presents a numbered list of preset options
#   plus a "custom value" entry, so the user always picks from a list.
#   The first option is the default (chosen on Enter or when non-interactive).
#   Usage: select_env <var> <label> <opt1> <opt2> ...
# ---------------------------------------------------------------
select_env() {
	local var="$1" label="$2"
	shift 2
	local -a opts=("$@")
	local default="${opts[0]}"

	# Already provided by the user → respect it, do not prompt.
	if [ -n "${!var:-}" ]; then
		echo -e "  ${GREEN}✓ ${label} (from \$${var}): ${!var}${NC}"
		return 0
	fi

	# No interactive terminal → accept the default silently.
	if [ ! -t 0 ]; then
		export "${var}=${default}"
		echo -e "  ${GREEN}✓ ${label}: ${default} ${CYAN}(default)${NC}"
		return 0
	fi

	echo -e "${YELLOW}${label}:${NC}"
	local i
	for i in "${!opts[@]}"; do
		if [ "$i" -eq 0 ]; then
			printf "  ${GREEN}%2d)${NC} %s ${CYAN}(default)${NC}\n" $((i + 1)) "${opts[$i]}"
		else
			printf "  ${GREEN}%2d)${NC} %s\n" $((i + 1)) "${opts[$i]}"
		fi
	done
	local custom_idx=$((${#opts[@]} + 1))
	printf "  ${GREEN}%2d)${NC} %s\n" "$custom_idx" "Enter a custom value"

	local choice
	while true; do
		read -r -p "$(echo -e "${YELLOW}Select [1-${custom_idx}, Enter=default]: ${NC}")" choice
		if [ -z "$choice" ]; then
			export "${var}=${default}"
			break
		fi
		if [[ "$choice" =~ ^[0-9]+$ ]]; then
			if [ "$choice" -ge 1 ] && [ "$choice" -le "${#opts[@]}" ]; then
				export "${var}=${opts[$((choice - 1))]}"
				break
			elif [ "$choice" -eq "$custom_idx" ]; then
				local custom
				read -r -p "$(echo -e "${YELLOW}Enter ${label}: ${NC}")" custom
				if [ -n "$custom" ]; then
					export "${var}=${custom}"
					break
				fi
				echo -e "${RED}Empty input, please select again${NC}"
				continue
			fi
		fi
		echo -e "${RED}Invalid input, please select again${NC}"
	done
	echo -e "  ${GREEN}✓ ${label}: ${!var}${NC}"
}

# ---------------------------------------------------------------
# select_env_secret — list-based prompt for secret values. The user chooses
#   between keeping the built-in default and entering a custom value, so secrets
#   still go through a selectable list. The resolved value is NOT echoed back
#   (only a "(value hidden)" confirmation) to keep it out of the terminal
#   scrollback / CI logs; the final summary masks it the same way.
#   Usage: select_env_secret <var> <label> <default>
# ---------------------------------------------------------------
select_env_secret() {
	local var="$1" label="$2" default="$3"

	if [ -n "${!var:-}" ]; then
		echo -e "  ${GREEN}✓ ${label} (from \$${var}) ${CYAN}(value hidden)${NC}"
		return 0
	fi

	# No interactive terminal: refuse to silently fall back to the built-in,
	# publicly-known demo password (anyone reading this repo knows it). The
	# operator must set the var explicitly, or opt in to the insecure default
	# with TENCENTCLOUD_ALLOW_INSECURE_DEFAULTS=1 for a throwaway sandbox.
	if [ ! -t 0 ]; then
		if [ "${TENCENTCLOUD_ALLOW_INSECURE_DEFAULTS:-0}" = "1" ] ||
			[ "${TENCENTCLOUD_ALLOW_INSECURE_DEFAULTS:-0}" = "true" ]; then
			export "${var}=${default}"
			echo -e "  ${YELLOW}⚠ ${label}: using the built-in INSECURE default (TENCENTCLOUD_ALLOW_INSECURE_DEFAULTS=1)${NC}"
			return 0
		fi
		echo -e "${RED}✗ ${label} (\$${var}) is unset and there is no interactive terminal.${NC}" >&2
		echo -e "  ${YELLOW}Refusing to use the built-in, publicly-known default password for a non-interactive run.${NC}" >&2
		echo -e "  ${YELLOW}Set ${var} (see env.example), or export TENCENTCLOUD_ALLOW_INSECURE_DEFAULTS=1 to accept the insecure demo default.${NC}" >&2
		exit 1
	fi

	echo -e "${YELLOW}${label}:${NC}"
	printf "  ${GREEN}%2d)${NC} %s ${CYAN}(default)${NC}\n" 1 "Use the built-in default (${default})"
	printf "  ${GREEN}%2d)${NC} %s\n" 2 "Enter a custom value"

	local choice custom
	while true; do
		read -r -p "$(echo -e "${YELLOW}Select [1-2, Enter=default]: ${NC}")" choice
		case "$choice" in
		"" | 1)
			export "${var}=${default}"
			break
			;;
		2)
			read -r -p "$(echo -e "${YELLOW}Enter ${label}: ${NC}")" custom
			if [ -n "$custom" ]; then
				export "${var}=${custom}"
				break
			fi
			echo -e "${RED}Empty input, please select again${NC}"
			;;
		*)
			echo -e "${RED}Invalid input, please select again${NC}"
			;;
		esac
	done
	echo -e "  ${GREEN}✓ ${label} set ${CYAN}(value hidden)${NC}"
}

# ---------------------------------------------------------------
# prompt_deployment_env — confirm/override the deployment-configuration env vars
#   that would otherwise default silently. Zone / compute instance type /
#   compute-node count / TKE on-off keep their richer dedicated selectors
#   (select_zone / select_instance_type / select_compute_nodes / select_tke) and
#   are not repeated here; TKE version & node count are prompted from select_tke
#   (only when a cluster is actually being created). Advanced behaviour toggles
#   (VERBOSE / REINSTALL / RESET_DB / REDEPLOY_* / BUILD_IMAGES / LOCAL_BUNDLE /
#   PVM_KERNEL_* / SSH_PORT) stay env-only and are intentionally not prompted.
# ---------------------------------------------------------------
prompt_deployment_env() {
	banner "Deployment configuration"

	# Network exposure mode for cube-api / cube-proxy / cube-webui. Asked FIRST
	# because it is the most security-sensitive choice of the run. The default is
	# internal (VPC-only) CLBs — the safe default with no public exposure.
	# Runs before setup_env so the resolved value maps to TF_VAR_enable_public_network.
	# An explicit TENCENTCLOUD_ENABLE_PUBLIC_NETWORK takes precedence and skips the
	# prompt; a non-interactive shell keeps the internal default (variables.tf).
	if [ -n "${TENCENTCLOUD_ENABLE_PUBLIC_NETWORK:-}" ]; then
		echo -e "  ${GREEN}✓ Public-network mode (from \$TENCENTCLOUD_ENABLE_PUBLIC_NETWORK): ${TENCENTCLOUD_ENABLE_PUBLIC_NETWORK}${NC}"
	elif [ -t 0 ]; then
		echo -e "${YELLOW}Network exposure for cube-api / cube-proxy / cube-webui:${NC}"
		echo -e "  ${YELLOW}- ${CYAN}No${YELLOW}  (default): VPC-internal CLBs, reachable only via the jumpserver / VPN (no public exposure).${NC}"
		echo -e "  ${YELLOW}- ${CYAN}Yes${YELLOW}: PUBLIC CLBs reachable from the internet. WebUI ships with no auth and cube-api${NC}"
		echo -e "  ${YELLOW}       passes all requests by default — harden them before exposing (see the deployment doc).${NC}"
		local _pub
		read -r -p "$(echo -e "${YELLOW}Expose these services via PUBLIC-network CLBs? [y/N]: ${NC}")" _pub
		case "${_pub}" in
		[Yy] | [Yy][Ee][Ss])
			export TENCENTCLOUD_ENABLE_PUBLIC_NETWORK=true
			echo -e "  ${GREEN}✓ Public-network mode: on ${YELLOW}(services will be reachable from the internet)${NC}"
			;;
		*)
			export TENCENTCLOUD_ENABLE_PUBLIC_NETWORK=false
			echo -e "  ${GREEN}✓ Public-network mode: off ${CYAN}(default; VPC-internal only)${NC}"
			;;
		esac
	fi

	select_env TENCENTCLOUD_REGION "Tencent Cloud region" \
		"ap-guangzhou" "ap-shanghai" "ap-beijing" "ap-nanjing" "ap-chengdu" \
		"ap-chongqing" "ap-hongkong" "ap-singapore" "ap-tokyo" "ap-seoul"
	echo -e "  ${YELLOW}Note: the region determines which availability zones and instance types are${NC}"
	echo -e "  ${YELLOW}      available; the zone / instance-type menus below are queried live for it.${NC}"
	select_env TENCENTCLOUD_VPC_NAME "VPC name" "cubesandbox-terraform-vpc"
	# OS image is fixed to OpenCloudOS Server 9: it is currently the only image
	# validated end-to-end (PVM kernel replacement, etc.), so it is not offered as
	# a user-selectable option. An explicit TENCENTCLOUD_IMAGE_NAME still overrides.
	if [ -n "${TENCENTCLOUD_IMAGE_NAME:-}" ]; then
		echo -e "  ${GREEN}✓ OS image name (from \$TENCENTCLOUD_IMAGE_NAME): ${TENCENTCLOUD_IMAGE_NAME}${NC}"
	else
		export TENCENTCLOUD_IMAGE_NAME="OpenCloudOS Server 9"
		echo -e "  ${GREEN}✓ OS image name: ${TENCENTCLOUD_IMAGE_NAME} ${CYAN}(fixed; only validated image)${NC}"
	fi
	select_env TENCENTCLOUD_JUMPSERVER_INSTANCE_TYPE "Jumpserver instance type" \
		"SA9.MEDIUM4" "SA9.MEDIUM8" "SA9.LARGE8" "SA5.MEDIUM4" "SA5.MEDIUM8"
	select_env_secret TENCENTCLOUD_MYSQL_PASSWORD "MySQL root password" "CubeSandbox123!"
	select_env_secret TENCENTCLOUD_REDIS_PASSWORD "Redis password" "ceuhvu123"
	select_env TENCENTCLOUD_CUBE_DB "Cube database name" "cube_mvp"
	select_env TENCENTCLOUD_CUBE_USER "Cube database user" "cube"
	select_env_secret TENCENTCLOUD_CUBE_PASSWORD "Cube database password" "cube_pass"
	select_env TENCENTCLOUD_CUBE_IMAGE_TAG "Cube component image tag" "v0.5.1" "dev"

	# Ask whether to print verbose terraform logs (defaults to off). Runs before
	# setup_env so the resolved value feeds VERBOSE. An explicit
	# TENCENTCLOUD_VERBOSE still takes precedence and skips the prompt.
	if [ -z "${TENCENTCLOUD_VERBOSE:-}" ] && [ -t 0 ]; then
		local _vlog
		read -r -p "$(echo -e "${YELLOW}Show verbose terraform logs (recommended)? [y/N]: ${NC}")" _vlog
		case "${_vlog}" in
		[Yy] | [Yy][Ee][Ss])
			export TENCENTCLOUD_VERBOSE=1
			echo -e "  ${GREEN}✓ Verbose terraform logs: on${NC}"
			;;
		*)
			export TENCENTCLOUD_VERBOSE=0
			echo -e "  ${GREEN}✓ Verbose terraform logs: off ${CYAN}(default)${NC}"
			;;
		esac
	fi
}

# ---------------------------------------------------------------
# _prompt_tke_env — confirm/override the TKE-specific env vars, invoked from
#   select_tke only when a TKE cluster will be created. setup_env already ran by
#   this point, so the resolved values are mapped to TF_VAR_* here as well (this
#   also ensures the defaults take effect, not just explicitly-set values).
# ---------------------------------------------------------------
_prompt_tke_env() {
	# TKE Kubernetes version is fixed and not prompted; show an informational
	# message only. An explicit TENCENTCLOUD_TKE_CLUSTER_VERSION still overrides.
	if [ -n "${TENCENTCLOUD_TKE_CLUSTER_VERSION:-}" ]; then
		echo -e "  ${GREEN}✓ TKE Kubernetes version (from \$TENCENTCLOUD_TKE_CLUSTER_VERSION): ${TENCENTCLOUD_TKE_CLUSTER_VERSION}${NC}"
	else
		export TENCENTCLOUD_TKE_CLUSTER_VERSION="1.34.1"
		echo -e "  ${GREEN}✓ TKE Kubernetes version: ${TENCENTCLOUD_TKE_CLUSTER_VERSION}${NC}"
	fi
	select_env TENCENTCLOUD_TKE_NODE_COUNT "TKE worker node count" "2" "1" "3" "4" "5"
	export TF_VAR_tke_cluster_version="$TENCENTCLOUD_TKE_CLUSTER_VERSION"
	export TF_VAR_tke_node_count="$TENCENTCLOUD_TKE_NODE_COUNT"
	TKE_CLUSTER_VERSION="$TENCENTCLOUD_TKE_CLUSTER_VERSION"
	TKE_NODE_COUNT="$TENCENTCLOUD_TKE_NODE_COUNT"
}

# ---------------------------------------------------------------
# Extract data source results from the JSON output of terraform plan
# ---------------------------------------------------------------
# Cache so the metadata plan runs at most once per create.sh invocation.
_TF_PLAN_JSON_CACHE=""
terraform_plan_json() {
	# Run terraform plan and output the plan as JSON
	# Return the JSON text.
	#
	# Both query_zones and query_instance_types read the SAME metadata plan (its
	# _zones / _instance_types outputs), so cache it: the region is already fixed
	# before either selector runs, so re-planning would just repeat a slow,
	# identical cloud round-trip.
	if [ -n "${_TF_PLAN_JSON_CACHE}" ]; then
		printf '%s' "${_TF_PLAN_JSON_CACHE}"
		return 0
	fi

	# mktemp (0600) instead of a predictable /tmp/...$$ path: the plan file holds
	# every root variable in plaintext (MySQL/Redis/cube passwords), so it must
	# not be world-readable or pre-creatable as a symlink by another local user.
	local planfile
	planfile="$(mktemp "${TMPDIR:-/tmp}/tfplan_cubesandbox.XXXXXX")"

	# This is a METADATA-ONLY plan: it exists solely to read the _zones /
	# _instance_types outputs (query_outputs.tf), which depend only on data
	# sources. Force the TKE / addons toggles OFF so the kubernetes provider is
	# never engaged here — otherwise it would try to reach a TKE cluster that does
	# not exist yet (config_path .kube/config) and fail the whole plan. The base
	# resources are still planned but never applied (the planfile is discarded).
	# Use -refresh=false so an existing kubernetes_* state does not try to refresh
	# through a stale 127.0.0.1 apiserver tunnel during zone/type discovery.
	local -a _meta_vars=(-var "create_tke=false" -var "deploy_tke_addons=false")
	local -a _meta_targets=(
		-target=data.tencentcloud_availability_zones_by_product.default
		-target=data.tencentcloud_instance_types.spec_8c16g
		-target=data.tencentcloud_instance_types.spec_4c8g
		-target=data.tencentcloud_instance_types.spec_2c4g
	)

	# Capture stderr from THIS plan so a failure can be surfaced without running a
	# second (slow) plan just to reproduce the error. mktemp keeps it 0600.
	local _errfile
	_errfile="$(mktemp "${TMPDIR:-/tmp}/tfplan_cubesandbox_err.XXXXXX")"
	if terraform plan -refresh=false -out="${planfile}" -input=false -no-color "${_meta_vars[@]}" "${_meta_targets[@]}" >/dev/null 2>"${_errfile}"; then
		local _json
		_json="$(terraform show -json "${planfile}" 2>/dev/null || true)"
		rm -f "${planfile}" "${_errfile}"
		_TF_PLAN_JSON_CACHE="${_json}"
		printf '%s' "${_json}"
	else
		# A copied deployment directory can carry a stale terraform.tfstate whose
		# cloud resources were already deleted. Metadata planning runs before the
		# normal phased apply retry logic, so prune confirmed-not-found addresses
		# here and retry once before surfacing the error.
		if grep -qiE 'not[ -]?found|does not exist|NotFound|ResourceNotFound|CdbInstanceNotFound|not exist' "$_errfile" 2>/dev/null; then
			local _stale_addrs _addr _pruned=0
			_stale_addrs="$(
				grep -E 'with [a-z][A-Za-z0-9_]*\.' "$_errfile" 2>/dev/null |
					sed -E 's/.*with ([^,]+),.*/\1/' | sort -u
			)"
			if [ -n "$_stale_addrs" ]; then
				echo -e "  ${YELLOW}Metadata plan found resources already gone in cloud; pruning stale state and retrying...${NC}" >&2
				while IFS= read -r _addr; do
					[ -n "$_addr" ] || continue
					echo -e "  ${CYAN}terraform state rm ${_addr}${NC}" >&2
					terraform state rm "$_addr" >/dev/null 2>&1 && _pruned=1 || true
				done <<EOF
${_stale_addrs}
EOF
				if [ "$_pruned" = "1" ]; then
					rm -f "${planfile}" "${_errfile}"
					_TF_PLAN_JSON_CACHE=""
					terraform_plan_json
					return $?
				fi
			fi
		fi
		# Show the captured error from the same failed plan (to stderr, so it does
		# not pollute the JSON this function prints on stdout).
		echo "" >&2
		echo -e "${RED}terraform plan failed, please check credentials and network${NC}" >&2
		cat "${_errfile}" >&2 2>/dev/null || true
		rm -f "${planfile}" "${_errfile}"
		return 1
	fi
}

# ---------------------------------------------------------------
# _remind_availability — resource availability is region/zone-specific. Printed
#   before the zone / instance-type menus so the user understands the list
#   reflects the configured region and that the final pick is still validated at
#   apply time (with automatic zone/instance-type fallback).
# ---------------------------------------------------------------
_remind_availability() {
	echo -e "  ${YELLOW}Note: availability varies by region AND availability zone — a zone or instance${NC}"
	echo -e "  ${YELLOW}      type offered in one place may be sold out or absent in another. The final${NC}"
	echo -e "  ${YELLOW}      choice is verified at apply time (create.sh retries with fallback if needed).${NC}"
}

# ---------------------------------------------------------------
# Query the list of availability zones
# ---------------------------------------------------------------
query_zones() {
	# jq is required to parse JSON
	if ! command -v jq &>/dev/null; then
		echo -e "${YELLOW}⚠ jq not installed, cannot auto-query availability zones. Please set TENCENTCLOUD_AVAILABILITY_ZONE manually${NC}"
		return 1
	fi

	echo -e "  ${CYAN}Querying availability zones...${NC}"

	local json
	json=$(terraform_plan_json) || return 1

	echo "$json" | jq -r '
    .planned_values.outputs._zones.value[]
    // empty
    | "\(.name)|\(.description // .name)"
  ' 2>/dev/null
}

# ---------------------------------------------------------------
# Query the list of instance types
# ---------------------------------------------------------------
query_instance_types() {
	if ! command -v jq &>/dev/null; then
		echo -e "${YELLOW}⚠ jq not installed, cannot auto-query instance types. Please set TENCENTCLOUD_COMPUTE_INSTANCE_TYPE manually${NC}"
		return 1
	fi

	echo -e "  ${CYAN}Querying instance types...${NC}"

	local json
	json=$(terraform_plan_json) || return 1

	echo "$json" | jq -r '
    .planned_values.outputs._instance_types.value[]
    // empty
    | "\(.type)|\(.cpu)|\(.memory)"
  ' 2>/dev/null | sort -t'|' -k1 -u
}

# ---------------------------------------------------------------
# Check whether the instance type is compatible with PVM deployment
# ---------------------------------------------------------------
check_pvm_compat() {
	local instance_type="$1"

	# S1.Xxx / S2.Xxx do not support PVM
	if [[ "$instance_type" =~ ^(S1|S2)\. ]]; then
		echo ""
		_draw_box "${RED}" \
			"PVM does not support ${instance_type}" \
			"PVM deployment does not support models below S3 (S1/S2)" \
			"Please choose S3/S4/S5/SA2/SA3 series"
		echo ""

		# Do not auto-exit, give the user a chance to re-select (triggers a retry when step2_apply fails)
		echo -e "  ${YELLOW}You can:${NC}"
		echo -e "    ${GREEN}1)${NC} Continue creating (may fail)"
		echo -e "    ${GREEN}2)${NC} Ctrl+C to exit, then reset ${CYAN}TENCENTCLOUD_COMPUTE_INSTANCE_TYPE${NC} and retry"
		echo -e "    ${GREEN}3)${NC} Wait until creation fails, then re-select during the retry flow"
		echo ""
	fi
}

# ---------------------------------------------------------------
# Ask for the number of compute nodes (before purchase, to allow parallel creation)
# ---------------------------------------------------------------
select_compute_nodes() {
	if [ -n "${COMPUTE_NODE_COUNT:-}" ]; then
		export TF_VAR_compute_node_count="$COMPUTE_NODE_COUNT"
		echo -e "${GREEN}✓ Compute node count (from environment variable): ${COMPUTE_NODE_COUNT}${NC}"
		return 0
	fi

	# No interactive terminal (CI / non-interactive run): cannot prompt, so
	# default to 2 (matches env.example / variables.tf). Set
	# TENCENTCLOUD_COMPUTE_NODE_COUNT to choose explicitly.
	if [ ! -t 0 ]; then
		export TF_VAR_compute_node_count=2
		echo -e "${YELLOW}⚠ No interactive terminal; defaulting compute node count to 2.${NC}"
		echo -e "  ${YELLOW}Set TENCENTCLOUD_COMPUTE_NODE_COUNT to choose explicitly.${NC}"
		return 0
	fi

	# Prompt for the number of compute nodes (at least 1; Enter keeps the
	# default of 2). Set TENCENTCLOUD_COMPUTE_NODE_COUNT to skip this prompt.
	echo ""
	local count
	while true; do
		read -r -p "$(echo -e "${YELLOW}Number of compute nodes to create [${CYAN}2${YELLOW}]: ${NC}")" count
		count="${count:-2}"
		if [[ "$count" =~ ^[0-9]+$ ]] && [ "$count" -ge 1 ]; then
			break
		fi
		echo -e "${RED}Please enter a positive integer (>= 1)${NC}"
	done
	export TF_VAR_compute_node_count="$count"
	echo -e "${GREEN}✓ Will create ${count} compute node(s)${NC}"
}

# ---------------------------------------------------------------
# Announce the TKE cluster (always created) and prompt its env (before purchase)
# ---------------------------------------------------------------
select_tke() {
	# The TKE Kubernetes cluster is always created; this is informational only.
	# TF_VAR_create_tke is turned on here so the build/addons stages treat this as
	# a TKE run (the phased apply later keeps it OFF for the base applies and flips
	# it back on for the final cluster + addons step).
	echo ""
	echo -e "${GREEN}✓ Creating the TKE Kubernetes cluster${NC}"
	echo -e "  Spec: standard cluster L5 | K8s ${TKE_CLUSTER_VERSION:-1.34.1} | ${TKE_NODE_COUNT:-2} nodes | preferred ${TF_VAR_tke_worker_instance_type:-SA9.LARGE8}"
	echo ""
	export TF_VAR_create_tke=true
	_prompt_tke_env
}

# ---------------------------------------------------------------
# Interactively select an availability zone
# ---------------------------------------------------------------
select_zone() {
	# If the user already set the environment variable, use it directly
	if [ -n "${TENCENTCLOUD_AVAILABILITY_ZONE:-}" ]; then
		echo -e "${GREEN}✓ Availability zone (from environment variable): ${TENCENTCLOUD_AVAILABILITY_ZONE}${NC}"
		_init_cvm_zones
		return 0
	fi

	# No interactive terminal (CI / non-interactive run): we cannot prompt, so
	# pick a sensible default instead of spinning forever on an EOF `read`.
	# Prefer the first live-queried zone, otherwise <region>-3. terraform apply
	# validates the actual zone; set TENCENTCLOUD_AVAILABILITY_ZONE to override.
	if [ ! -t 0 ]; then
		local _region _zfirst _zlist
		_region="${TF_VAR_region:-${TENCENTCLOUD_REGION:-ap-guangzhou}}"
		_zlist=$(query_zones 2>/dev/null) || _zlist=""
		_zfirst=$(printf '%s\n' "$_zlist" | head -1 | cut -d'|' -f1)
		[ -z "$_zfirst" ] && _zfirst="${TENCENTCLOUD_AVAILABILITY_ZONE:-ap-guangzhou-6}"
		export TF_VAR_availability_zone="$_zfirst"
		_init_cvm_zones
		echo -e "${YELLOW}⚠ No interactive terminal; defaulting availability zone to ${_zfirst}.${NC}"
		echo -e "  ${YELLOW}Set TENCENTCLOUD_AVAILABILITY_ZONE to choose explicitly.${NC}"
		return 0
	fi

	echo ""
	echo -e "${YELLOW}Availability zone not set, querying the list...${NC}"
	_remind_availability

	# Store the results in arrays
	local -a zone_names=()
	local -a zone_descs=()

	local zones
	zones=$(query_zones) || zones=""

	if [ -n "$zones" ]; then
		while IFS='|' read -r name desc; do
			[ -n "$name" ] || continue
			zone_names+=("$name")
			zone_descs+=("$desc")
		done <<<"$zones"
	fi

	# Fallback: auto-query failed or returned nothing. Instead of forcing the user
	# to set TENCENTCLOUD_AVAILABILITY_ZONE manually, generate candidate zones from
	# the region (region-1 .. region-8) so the user can still pick from a list.
	# terraform apply will validate the actual availability of the chosen zone.
	local is_fallback=0
	if [ "${#zone_names[@]}" -eq 0 ]; then
		is_fallback=1
		local region="${TF_VAR_region:-${TENCENTCLOUD_REGION:-ap-guangzhou}}"
		echo -e "${YELLOW}Auto-query failed; generated candidate zones from region ${region}${NC}"
		local n
		for n in 1 2 3 4 5 6 7 8; do
			zone_names+=("${region}-${n}")
			zone_descs+=("")
		done
	fi

	echo ""
	echo -e "${CYAN}Available availability zones:${NC}"
	if [ "$is_fallback" -eq 1 ]; then
		echo -e "${YELLOW}(candidate zones; actual availability verified at creation time)${NC}"
	fi
	local i
	for i in "${!zone_names[@]}"; do
		printf "  ${GREEN}%2d)${NC} %-20s %s\n" $((i + 1)) "${zone_names[$i]}" "${zone_descs[$i]}"
	done
	printf "  ${GREEN}%2d)${NC} %s\n" 0 "Enter a custom availability zone"
	echo ""

	# User selection
	local choice
	while true; do
		read -r -p "$(echo -e "${YELLOW}Select an availability zone [0-${#zone_names[@]}]: ${NC}")" choice
		if [ "$choice" = "0" ]; then
			local custom
			read -r -p "$(echo -e "${YELLOW}Enter an availability zone (e.g. ${TF_VAR_region:-${TENCENTCLOUD_REGION:-ap-guangzhou}}-1): ${NC}")" custom
			if [ -n "$custom" ]; then
				export TF_VAR_availability_zone="$custom"
				_init_cvm_zones
				echo -e "${GREEN}✓ Selected availability zone: ${custom}${NC}"
				return 0
			fi
			echo -e "${RED}Empty input, please select again${NC}"
			continue
		fi
		if [[ "$choice" =~ ^[0-9]+$ ]] && [ "$choice" -ge 1 ] && [ "$choice" -le "${#zone_names[@]}" ]; then
			break
		fi
		echo -e "${RED}Invalid input, please select again${NC}"
	done

	local selected="${zone_names[$((choice - 1))]}"
	export TF_VAR_availability_zone="$selected"
	_init_cvm_zones
	echo -e "${GREEN}✓ Selected availability zone: ${selected}${NC}"
}

# ---------------------------------------------------------------
# Query and populate the global list of available instance types
# ---------------------------------------------------------------
_fetch_instance_types() {
	echo ""
	echo -e "${YELLOW}Instance type not set, querying the list...${NC}"

	local types
	types=$(query_instance_types) || return 1

	if [ -z "$types" ]; then
		echo -e "${RED}No instance types found, please check the region/availability-zone settings${NC}"
		return 1
	fi

	# Filter: keep only recommended configs (CPU >= 4, RAM >= 8) and exclude S1/S2
	CVM_TYPES=()
	CVM_CPUS=()
	CVM_MEMS=()

	while IFS='|' read -r t cpu mem; do
		[ -z "$t" ] && continue
		[[ "$cpu" =~ ^[0-9]+$ ]] || continue
		[[ "$mem" =~ ^[0-9]+$ ]] || continue
		[[ "$t" =~ ^(S1|S2)\. ]] && continue
		[ "$cpu" -lt 4 ] && continue
		[ "$mem" -lt 8 ] && continue
		CVM_TYPES+=("$t")
		CVM_CPUS+=("$cpu")
		CVM_MEMS+=("$mem")
	done <<<"$types"

	if [ ${#CVM_TYPES[@]} -eq 0 ]; then
		echo -e "${RED}No instance types matching the recommended config were found${NC}"
		echo -e "${RED}(CPU >= 4, RAM >= 8, S3+ series)${NC}"
		return 1
	fi
	return 0
}

# ---------------------------------------------------------------
# _fallback_instance_types — populate a curated candidate list when the
#   auto-query fails (e.g. plan/network error or jq missing). These are common
#   Tencent Cloud types that satisfy the recommended config (CPU >= 4, RAM >= 8,
#   S3+ series). Actual availability in the chosen zone is verified at apply time;
#   the selection menu also keeps a manual-input option.
# ---------------------------------------------------------------
_fallback_instance_types() {
	[ "${1:-}" = "quiet" ] || echo -e "${YELLOW}Auto-query failed; showing a curated list of candidate instance types${NC}"
	CVM_TYPES=()
	CVM_CPUS=()
	CVM_MEMS=()

	local -a fb=(
		"SA9.LARGE8|4|8"
		"SA9.LARGE16|4|16"
		"SA9.2XLARGE16|8|16"
		"SA9.2XLARGE32|8|32"
		"SA9.4XLARGE32|16|32"
		"SA5.2XLARGE16|8|16"
	)
	local entry t cpu mem
	for entry in "${fb[@]}"; do
		IFS='|' read -r t cpu mem <<<"$entry"
		CVM_TYPES+=("$t")
		CVM_CPUS+=("$cpu")
		CVM_MEMS+=("$mem")
	done
	return 0
}

_has_existing_instance_config() {
	local compute_types state_count
	compute_types="${TF_VAR_compute_instance_types:-}"
	if [ -n "$compute_types" ] && [ "$compute_types" != "[]" ] && printf '%s' "$compute_types" | jq -e 'length > 0' >/dev/null 2>&1; then
		return 0
	fi
	state_count="$(terraform state list 2>/dev/null | grep -cE '^(tencentcloud_instance\.compute\[|tencentcloud_kubernetes_cluster\.tke\[0\])' || true)"
	[ "${state_count:-0}" -gt 0 ] 2>/dev/null
}

_use_existing_instance_config_for_rerun() {
	_has_existing_instance_config || return 1
	echo -e "  ${GREEN}✓ Existing compute/TKE resources or resolved instance config detected; skipping online instance-type query.${NC}"
	echo -e "  ${CYAN}  Reusing purchased/resolved instance types; curated fallback candidates are kept only for any new retry.${NC}"
	_fallback_instance_types quiet
	return 0
}

# ---------------------------------------------------------------
# _fallback_jumpserver_instance_types — curated jumpserver candidates
# ---------------------------------------------------------------
_fallback_jumpserver_instance_types() {
	JUMPSERVER_TYPES=("SA9.MEDIUM4" "SA9.MEDIUM8" "SA9.LARGE8" "SA5.MEDIUM4" "SA5.MEDIUM8")
	JUMPSERVER_CPUS=(2 2 4 2 2)
	JUMPSERVER_MEMS=(4 8 8 4 8)
}

# ---------------------------------------------------------------
# _init_cvm_zones — default per-role zones to the primary zone when unset
# ---------------------------------------------------------------
_init_cvm_zones() {
	local primary="${TF_VAR_availability_zone:-}"
	[ -n "$primary" ] || return 0
	[ -z "${TF_VAR_jumpserver_availability_zone:-}" ] && export TF_VAR_jumpserver_availability_zone="$primary"
	[ -z "${TF_VAR_compute_availability_zone:-}" ] && export TF_VAR_compute_availability_zone="$primary"
	[ -z "${TF_VAR_tke_worker_availability_zone:-}" ] && export TF_VAR_tke_worker_availability_zone="$primary"
	return 0
}

# ---------------------------------------------------------------
# _build_fallback_zones — populate _FALLBACK_ZONES for auto-fallback.
# Uses a global array instead of bash 4 nameref so macOS bash 3.2 works.
# ---------------------------------------------------------------
_FALLBACK_ZONES=()
_build_fallback_zones() {
	local _region="${TF_VAR_region:-${TENCENTCLOUD_REGION:-ap-guangzhou}}" _seen=" " _role_zone _zn _z

	_FALLBACK_ZONES=()
	for _role_zone in \
		"${TF_VAR_jumpserver_availability_zone:-}" \
		"${TF_VAR_compute_availability_zone:-}" \
		"${TF_VAR_tke_worker_availability_zone:-}" \
		"${TF_VAR_availability_zone:-}"; do
		[ -n "$_role_zone" ] || continue
		case "$_seen" in
		*" ${_role_zone} "*) ;;
		*)
			_FALLBACK_ZONES+=("$_role_zone")
			_seen="${_seen}${_role_zone} "
			;;
		esac
	done
	for _zn in 1 2 3 4 5 6 7 8; do
		_z="${_region}-${_zn}"
		case "$_seen" in
		*" ${_z} "*) ;;
		*)
			_FALLBACK_ZONES+=("$_z")
			_seen="${_seen}${_z} "
			;;
		esac
	done
}

# ---------------------------------------------------------------
# _ensure_cvm_subnet_for_zone — create the extra subnet for a non-primary zone
# ---------------------------------------------------------------
_ensure_cvm_subnet_for_zone() {
	local zone="$1"
	local primary="${TF_VAR_availability_zone:-}"
	[ -n "$zone" ] || return 0
	[ -n "$primary" ] && [ "$zone" = "$primary" ] && return 0

	local target="tencentcloud_subnet.cvm[\"${zone}\"]"
	if terraform state list 2>/dev/null | grep -Fq "$target"; then
		return 0
	fi

	echo -e "  ${CYAN}Creating VPC subnet in ${zone} for cross-zone CVM placement...${NC}"
	_apply_phase "Step: Create subnet in ${zone}" "$target" || return 1
}

# ---------------------------------------------------------------
# _parse_failed_cvm_resource — infer which CVM role failed from apply log
#   Echoes: jumpserver | compute | tke | unknown
# ---------------------------------------------------------------
_parse_failed_cvm_resource() {
	local log="$1"
	if grep -q 'with tencentcloud_instance\.jumpserver' "$log" 2>/dev/null; then
		echo "jumpserver"
	elif grep -q 'with tencentcloud_instance\.compute' "$log" 2>/dev/null; then
		echo "compute"
	elif grep -qE 'with tencentcloud_kubernetes_cluster\.tke|with tencentcloud_kubernetes_node_pool\.tke' "$log" 2>/dev/null; then
		echo "tke"
	else
		echo "unknown"
	fi
}

# ---------------------------------------------------------------
# _role_zone_var — echo the TF_VAR_* name for a CVM role's zone
# ---------------------------------------------------------------
_role_zone_var() {
	case "$1" in
	jumpserver) echo "TF_VAR_jumpserver_availability_zone" ;;
	compute) echo "TF_VAR_compute_availability_zone" ;;
	tke) echo "TF_VAR_tke_worker_availability_zone" ;;
	*) echo "" ;;
	esac
}

# ---------------------------------------------------------------
# _compute_preferred_type — user preference (TENCENTCLOUD_COMPUTE_INSTANCE_TYPE)
# ---------------------------------------------------------------
_compute_preferred_type() {
	echo "${COMPUTE_PREFERRED_TYPE:-${TF_VAR_compute_instance_type:-${TENCENTCLOUD_COMPUTE_INSTANCE_TYPE:-SA9.MEDIUM8}}}"
}

# ---------------------------------------------------------------
# _compute_preferred_zone — default zone for not-yet-purchased compute nodes
# ---------------------------------------------------------------
_compute_preferred_zone() {
	echo "${COMPUTE_PREFERRED_ZONE:-${TF_VAR_compute_availability_zone:-${TF_VAR_availability_zone:-}}}"
}

# ---------------------------------------------------------------
# _json_string_array — encode bash array elements as a JSON string array
# ---------------------------------------------------------------
_json_string_array() {
	local -a items=("$@")
	if [ "${#items[@]}" -eq 0 ]; then
		echo "[]"
		return 0
	fi
	printf '%s\n' "${items[@]}" | jq -R . | jq -s -c .
}

# ---------------------------------------------------------------
# _read_compute_type_from_state — instance type of compute[i] from terraform state
# ---------------------------------------------------------------
_read_compute_type_from_state() {
	local idx="$1"
	terraform state show "tencentcloud_instance.compute[${idx}]" 2>/dev/null |
		awk -F' = ' '/^[[:space:]]*instance_type[[:space:]]+=/ { gsub(/"/, "", $2); print $2; exit }'
}

# ---------------------------------------------------------------
# _read_compute_zone_from_state — availability zone of compute[i] from state
# ---------------------------------------------------------------
_read_compute_zone_from_state() {
	local idx="$1"
	terraform state show "tencentcloud_instance.compute[${idx}]" 2>/dev/null |
		awk -F' = ' '/^[[:space:]]*availability_zone[[:space:]]+=/ { gsub(/"/, "", $2); print $2; exit }'
}

# ---------------------------------------------------------------
# _load_purchased_compute_config — hydrate per-node arrays from state/output
# ---------------------------------------------------------------
_load_purchased_compute_config() {
	local count="${TF_VAR_compute_node_count:-0}" i type zone
	COMPUTE_PURCHASED_TYPES=()
	COMPUTE_PURCHASED_ZONES=()

	local types_json zones_json
	types_json=$(terraform output -json compute_instance_types 2>/dev/null || echo "[]")
	zones_json=$(terraform output -json compute_availability_zones 2>/dev/null || echo "[]")

	for ((i = 0; i < count; i++)); do
		type=$(printf '%s' "$types_json" | jq -r ".[$i] // empty" 2>/dev/null || echo "")
		zone=$(printf '%s' "$zones_json" | jq -r ".[$i] // empty" 2>/dev/null || echo "")
		if [ -z "$type" ] && terraform state list 2>/dev/null | grep -Fq "tencentcloud_instance.compute[${i}]"; then
			type=$(_read_compute_type_from_state "$i")
		fi
		if [ -z "$zone" ] && terraform state list 2>/dev/null | grep -Fq "tencentcloud_instance.compute[${i}]"; then
			zone=$(_read_compute_zone_from_state "$i")
		fi
		COMPUTE_PURCHASED_TYPES[$i]="${type:-}"
		COMPUTE_PURCHASED_ZONES[$i]="${zone:-}"
	done
}

# ---------------------------------------------------------------
# _export_compute_node_config_var — sync TF_VAR_compute_instance_types and
#   TF_VAR_compute_availability_zones for the upcoming apply attempt.
# ---------------------------------------------------------------
_export_compute_node_config_var() {
	local idx="$1" count="$2"
	local preferred_type preferred_zone
	local -a types=() zones=()
	local i

	preferred_type=$(_compute_preferred_type)
	preferred_zone=$(_compute_preferred_zone)

	for ((i = 0; i < count; i++)); do
		if [ "$i" -lt "$idx" ] && [ -n "${COMPUTE_PURCHASED_TYPES[$i]:-}" ]; then
			types+=("${COMPUTE_PURCHASED_TYPES[$i]}")
		elif [ "$i" -eq "$idx" ]; then
			types+=("${TF_VAR_compute_instance_type:-$preferred_type}")
		else
			types+=("${preferred_type}")
		fi

		if [ "$i" -lt "$idx" ] && [ -n "${COMPUTE_PURCHASED_ZONES[$i]:-}" ]; then
			zones+=("${COMPUTE_PURCHASED_ZONES[$i]}")
		elif [ "$i" -eq "$idx" ]; then
			zones+=("${TF_VAR_compute_availability_zone:-$preferred_zone}")
		else
			zones+=("${preferred_zone}")
		fi
	done

	export TF_VAR_compute_instance_types="$(_json_string_array "${types[@]}")"
	export TF_VAR_compute_availability_zones="$(_json_string_array "${zones[@]}")"
}

# ---------------------------------------------------------------
# _sync_compute_config_from_state — on re-run, keep purchased node config
# ---------------------------------------------------------------
_sync_compute_config_from_state() {
	local count="${TF_VAR_compute_node_count:-0}" purchased=0 i
	[ "$count" -gt 0 ] 2>/dev/null || return 0
	_load_purchased_compute_config
	for ((i = 0; i < count; i++)); do
		[ -n "${COMPUTE_PURCHASED_TYPES[$i]:-}" ] && purchased=1
	done
	if [ "$purchased" -eq 1 ]; then
		export TF_VAR_compute_instance_types="$(_json_string_array "${COMPUTE_PURCHASED_TYPES[@]}")"
		export TF_VAR_compute_availability_zones="$(_json_string_array "${COMPUTE_PURCHASED_ZONES[@]}")"
	fi
}

# ---------------------------------------------------------------
# purchase_compute_nodes — buy each compute node independently; auto-fallback
#   may pick different eligible instance types / zones per node.
# ---------------------------------------------------------------
purchase_compute_nodes() {
	local count="$1" i
	local preferred_type preferred_zone

	preferred_type=$(_compute_preferred_type)
	preferred_zone=$(_compute_preferred_zone)
	COMPUTE_PREFERRED_TYPE="$preferred_type"
	COMPUTE_PREFERRED_ZONE="$preferred_zone"

	_load_purchased_compute_config

	for ((i = 0; i < count; i++)); do
		if terraform state list 2>/dev/null | grep -Fq "tencentcloud_instance.compute[${i}]"; then
			[ -z "${COMPUTE_PURCHASED_TYPES[$i]:-}" ] && COMPUTE_PURCHASED_TYPES[$i]=$(_read_compute_type_from_state "$i")
			[ -z "${COMPUTE_PURCHASED_ZONES[$i]:-}" ] && COMPUTE_PURCHASED_ZONES[$i]=$(_read_compute_zone_from_state "$i")
			continue
		fi

		CVM_TYPE_INDEX=-1
		COMPUTE_ZONE_INDEX=0
		export TF_VAR_compute_instance_type="$preferred_type"
		export TF_VAR_compute_availability_zone="$preferred_zone"

		STEP2_COMPUTE_NODE_INDEX=$i
		STEP2_LABEL="Step: Purchase compute node $((i + 1))/${count}"
		STEP2_TARGETS=("tencentcloud_instance.compute[${i}]")
		STEP2_CVM_FALLBACK=1
		step2_apply

		COMPUTE_PURCHASED_TYPES[$i]=$(_read_compute_type_from_state "$i")
		COMPUTE_PURCHASED_ZONES[$i]=$(_read_compute_zone_from_state "$i")
		echo -e "  ${GREEN}✓ Compute node $((i + 1)) purchased: ${COMPUTE_PURCHASED_TYPES[$i]} @ ${COMPUTE_PURCHASED_ZONES[$i]:-?}${NC}"
	done

	STEP2_COMPUTE_NODE_INDEX=-1
	STEP2_TARGETS=()
	STEP2_CVM_FALLBACK=0
	export TF_VAR_compute_instance_types="$(_json_string_array "${COMPUTE_PURCHASED_TYPES[@]}")"
	export TF_VAR_compute_availability_zones="$(_json_string_array "${COMPUTE_PURCHASED_ZONES[@]}")"
}

# ---------------------------------------------------------------
# _get_role_zone — read the current zone for a CVM role
# ---------------------------------------------------------------
_get_role_zone() {
	case "$1" in
	jumpserver) echo "${TF_VAR_jumpserver_availability_zone:-${TF_VAR_availability_zone:-}}" ;;
	compute)
		if [ "${STEP2_COMPUTE_NODE_INDEX:-}" -ge 0 ] 2>/dev/null; then
			echo "${TF_VAR_compute_availability_zone:-$(_compute_preferred_zone)}"
		else
			echo "${TF_VAR_compute_availability_zone:-${TF_VAR_availability_zone:-}}"
		fi
		;;
	tke) echo "${TF_VAR_tke_worker_availability_zone:-${TF_VAR_availability_zone:-}}" ;;
	*) echo "${TF_VAR_availability_zone:-}" ;;
	esac
}

# ---------------------------------------------------------------
# _set_role_zone — set the zone for a CVM role
# ---------------------------------------------------------------
_set_role_zone() {
	local role="$1" zone="$2" var
	var=$(_role_zone_var "$role")
	[ -n "$var" ] || return 1
	export "${var}=${zone}"
}

# ---------------------------------------------------------------
# _get_role_zone_index — read zone fallback index for a role
# ---------------------------------------------------------------
_get_role_zone_index() {
	case "$1" in
	jumpserver) echo "${JUMPSERVER_ZONE_INDEX:-0}" ;;
	compute) echo "${COMPUTE_ZONE_INDEX:-0}" ;;
	tke) echo "${TKE_ZONE_INDEX:-0}" ;;
	*) echo "0" ;;
	esac
}

# ---------------------------------------------------------------
# _set_role_zone_index — write zone fallback index for a role
# ---------------------------------------------------------------
_set_role_zone_index() {
	local role="$1" idx="$2"
	case "$role" in
	jumpserver) JUMPSERVER_ZONE_INDEX=$idx ;;
	compute) COMPUTE_ZONE_INDEX=$idx ;;
	tke) TKE_ZONE_INDEX=$idx ;;
	esac
}

# ---------------------------------------------------------------
# _switch_role_to_next_zone — move one CVM role to the next candidate zone
#   Returns 0 when a new zone was selected, 1 when exhausted.
# ---------------------------------------------------------------
_switch_role_to_next_zone() {
	local role="$1"
	local -a _zones=()
	_build_fallback_zones
	_zones=("${_FALLBACK_ZONES[@]+"${_FALLBACK_ZONES[@]}"}")
	[ "${#_zones[@]}" -gt 0 ] || return 1

	local _zi
	_zi=$(_get_role_zone_index "$role")
	_zi=$((_zi + 1))
	if [ "$_zi" -ge "${#_zones[@]}" ]; then
		return 1
	fi

	_set_role_zone_index "$role" "$_zi"
	_set_role_zone "$role" "${_zones[$_zi]}"
	_ensure_cvm_subnet_for_zone "${_zones[$_zi]}" || return 1
	if [ "$role" = "compute" ] && [ "${STEP2_COMPUTE_NODE_INDEX:-}" -ge 0 ] 2>/dev/null; then
		_export_compute_node_config_var "$STEP2_COMPUTE_NODE_INDEX" "${TF_VAR_compute_node_count:-2}"
	fi
	echo -e "  ${GREEN}-> Auto-switching ${role} zone: now trying $(_get_role_zone "$role")${NC}"
	return 0
}

# ---------------------------------------------------------------
# _try_next_instance_type — cycle instance type for the failed CVM role
#   Returns 0 when a new type was selected, 1 when exhausted.
# ---------------------------------------------------------------
_try_next_instance_type() {
	local role="$1"
	local next_idx next_type

	case "$role" in
	jumpserver)
		[ "${#JUMPSERVER_TYPES[@]}" -eq 0 ] && _fallback_jumpserver_instance_types
		next_idx=$((JUMPSERVER_TYPE_INDEX + 1))
		[ "$next_idx" -lt 0 ] 2>/dev/null && next_idx=0
		[ "$next_idx" -ge "${#JUMPSERVER_TYPES[@]}" ] && return 1
		JUMPSERVER_TYPE_INDEX=$next_idx
		next_type="${JUMPSERVER_TYPES[$next_idx]}"
		export TF_VAR_jumpserver_instance_type="$next_type"
		echo -e "  ${GREEN}-> Auto-fallback (jumpserver): trying ${next_type} (${JUMPSERVER_CPUS[$next_idx]}C${JUMPSERVER_MEMS[$next_idx]}G) [$(($next_idx + 1))/${#JUMPSERVER_TYPES[@]}]${NC}"
		;;
	compute | tke | unknown)
		[ "${#CVM_TYPES[@]}" -eq 0 ] && _fallback_instance_types
		next_idx=$((CVM_TYPE_INDEX + 1))
		[ "$next_idx" -lt 0 ] 2>/dev/null && next_idx=0
		[ "$next_idx" -ge "${#CVM_TYPES[@]}" ] && return 1
		CVM_TYPE_INDEX=$next_idx
		next_type="${CVM_TYPES[$next_idx]}"
		export TF_VAR_compute_instance_type="$next_type"
		if [ "$role" = "compute" ] && [ "${STEP2_COMPUTE_NODE_INDEX:-}" -ge 0 ] 2>/dev/null; then
			_export_compute_node_config_var "$STEP2_COMPUTE_NODE_INDEX" "${TF_VAR_compute_node_count:-2}"
		fi
		if [ "$role" = "tke" ]; then
			# tke_worker_instance_type is used directly (no ternary fallback), so we
			# must override it here to avoid retrying the sold-out type.
			export TF_VAR_tke_worker_instance_type="$next_type"
			echo -e "  ${GREEN}-> Auto-fallback (TKE workers): trying ${next_type} (${CVM_CPUS[$next_idx]}C${CVM_MEMS[$next_idx]}G) [$(($next_idx + 1))/${#CVM_TYPES[@]}]${NC}"
		else
			echo -e "  ${GREEN}-> Auto-fallback (compute): trying ${next_type} (${CVM_CPUS[$next_idx]}C${CVM_MEMS[$next_idx]}G) [$(($next_idx + 1))/${#CVM_TYPES[@]}]${NC}"
		fi
		;;
	*)
		return 1
		;;
	esac
	return 0
}

# ---------------------------------------------------------------
# _try_cvm_stock_fallback — same-zone instance-type fallback FIRST, then
#   cross-zone fallback, for one role.
#   Strategy: on a stock / availability failure, cycle through the REMAINING
#   candidate instance types in the CURRENT zone; only once every type has failed
#   here do we move to the next candidate zone (restarting the type list there).
#   The single exception is a genuinely unusable zone (retired/invalid, signalled
#   by zone_invalid=1, e.g. InvalidZone.MismatchRegion): no type can succeed there,
#   so skip straight to the next zone instead of cycling types in a dead one.
#   $1 = role, $2 = zone_invalid (1 = jump to the next zone immediately).
#   Returns 0 when config was adjusted and the caller should retry, 1 when every
#   candidate type in every candidate zone has been exhausted.
# ---------------------------------------------------------------
_try_cvm_stock_fallback() {
	local role="$1"
	local _zone_invalid=0
	[ "$2" = "1" ] && _zone_invalid=1

	# Unusable zone: no instance type will work here, so go straight to the next
	# zone instead of cycling every type in a dead zone.
	if [ "$_zone_invalid" = "1" ]; then
		JUMPSERVER_TYPE_INDEX=-1
		CVM_TYPE_INDEX=-1
		if _switch_role_to_next_zone "$role"; then
			echo -e "  ${YELLOW}Reason: previous zone unusable; switching to $(_get_role_zone "$role")${NC}"
			return 0
		fi
		return 1
	fi

	# Same-zone instance-type fallback FIRST: try the next candidate type in the
	# CURRENT zone before changing the zone.
	if _try_next_instance_type "$role"; then
		echo -e "  ${YELLOW}Reason: sold out / type unavailable in $(_get_role_zone "$role"); trying another type in the same zone${NC}"
		return 0
	fi

	# Every candidate type has failed in this zone → move to the next zone and
	# restart the type list there.
	JUMPSERVER_TYPE_INDEX=-1
	CVM_TYPE_INDEX=-1
	if _switch_role_to_next_zone "$role"; then
		echo -e "  ${YELLOW}All candidate instance types tried for ${role} in the previous zone; switching to $(_get_role_zone "$role")${NC}"
		return 0
	fi

	return 1
}

# Interactively select an instance type
# ---------------------------------------------------------------
select_instance_type() {
	# If the user/resolved config already set the instance type, use it directly.
	if [ -n "${TF_VAR_compute_instance_type:-${TENCENTCLOUD_COMPUTE_INSTANCE_TYPE:-}}" ]; then
		local _configured_type
		_configured_type="${TF_VAR_compute_instance_type:-${TENCENTCLOUD_COMPUTE_INSTANCE_TYPE:-}}"
		echo -e "${GREEN}✓ Compute node preferred instance type (from configuration): ${_configured_type}${NC}"
		echo -e "  ${CYAN}(Auto-fallback may purchase other eligible types per node if this type is sold out.)${NC}"
		export TF_VAR_compute_instance_type="$_configured_type"
		check_pvm_compat "$_configured_type"
		# On re-run, existing state/resolved.auto.tfvars already describes the
		# purchased shape; avoid metadata plans that can touch stale k8s state.
		_use_existing_instance_config_for_rerun || _fetch_instance_types || _fallback_instance_types
		return 0
	fi

	# No interactive terminal (CI / non-interactive run): cannot prompt, so use
	# the preferred default type instead of looping forever on an EOF `read`.
	# Per-node auto-fallback still kicks in at purchase time if it is sold out.
	# Set TENCENTCLOUD_COMPUTE_INSTANCE_TYPE to choose explicitly.
	if [ ! -t 0 ]; then
		local _t
		_t="$(_compute_preferred_type)"
		export TF_VAR_compute_instance_type="$_t"
		_use_existing_instance_config_for_rerun || _fetch_instance_types || _fallback_instance_types
		check_pvm_compat "$_t" || true
		echo -e "${YELLOW}⚠ No interactive terminal; defaulting compute instance type to ${_t}.${NC}"
		echo -e "  ${YELLOW}Set TENCENTCLOUD_COMPUTE_INSTANCE_TYPE to choose explicitly.${NC}"
		return 0
	fi

	if _use_existing_instance_config_for_rerun; then
		return 0
	fi

	echo ""
	# On query failure, fall back to a curated candidate list instead of exiting so
	# the user can still pick an instance type (or enter one manually).
	_fetch_instance_types || _fallback_instance_types

	echo ""
	echo -e "${CYAN}CubeSandbox recommended config: CPU ≥ 4 cores, RAM ≥ 8 GB${NC}"
	echo ""

	# PVM deployment reminder
	_draw_box "${YELLOW}" \
		"📌 Note: when a compute node uses CVM, it is deployed via PVM" \
		"PVM deployment does not yet support models below S3 (S1/S2 filtered out)"
	echo ""
	_remind_availability
	echo ""

	echo -e "${GREEN}Available instance types:${NC}"
	for i in "${!CVM_TYPES[@]}"; do
		printf "  ${GREEN}%2d)${NC} %-20s ${CYAN}%dC%dG${NC}\n" \
			$((i + 1)) "${CVM_TYPES[$i]}" "${CVM_CPUS[$i]}" "${CVM_MEMS[$i]}"
	done

	local manual_idx=$((${#CVM_TYPES[@]} + 1))
	printf "  ${GREEN}%2d)${NC} %s\n" "$manual_idx" "Manual input"
	echo ""

	# User selection
	local choice
	local max_choice=$manual_idx
	while true; do
		read -r -p "$(echo -e "${YELLOW}Select an instance type [1-${max_choice}]: ${NC}")" choice
		if [[ "$choice" =~ ^[0-9]+$ ]] && [ "$choice" -ge 1 ] && [ "$choice" -le "$max_choice" ]; then
			break
		fi
		echo -e "${RED}Invalid input, please select again${NC}"
	done

	if [ "$choice" -lt "$manual_idx" ]; then
		local selected="${CVM_TYPES[$((choice - 1))]}"
		export TF_VAR_compute_instance_type="$selected"
		CVM_TYPE_INDEX=$((choice - 1))
		echo -e "${GREEN}✓ Selected compute node instance type: ${selected} (${CVM_CPUS[$((choice - 1))]}C${CVM_MEMS[$((choice - 1))]}G)${NC}"
		check_pvm_compat "$selected"
	else
		# Manual input
		local manual
		echo ""
		echo -e "${CYAN}CVM types matching the criteria in the current availability zone (${TF_VAR_availability_zone:-auto}):${NC}"
		for i in "${!CVM_TYPES[@]}"; do
			printf "  ${GREEN}%2d)${NC} %-20s ${CYAN}%dC%dG${NC}\n" \
				$((i + 1)) "${CVM_TYPES[$i]}" "${CVM_CPUS[$i]}" "${CVM_MEMS[$i]}"
		done
		echo ""
		while true; do
			read -r -p "$(echo -e "${YELLOW}Enter an instance type (e.g. SA9.LARGE8): ${NC}")" manual
			if [ -n "$manual" ]; then
				break
			fi
			echo -e "${RED}Input cannot be empty${NC}"
		done
		export TF_VAR_compute_instance_type="$manual"
		echo -e "${GREEN}✓ Selected compute node instance type: ${manual}${NC}"
		check_pvm_compat "$manual"
	fi
}

# ---------------------------------------------------------------
# Step 1: terraform init
# ---------------------------------------------------------------
step1_init() {
	banner "Step: terraform init"
	# Use _tf_keep_stderr to keep error output, making it easier to diagnose init failures
	_tf_keep_stderr init -input=false || {
		echo -e "${RED}✗ terraform init failed, please check the provider config and network${NC}"
		exit 1
	}
	echo -e "${GREEN}✓ Initialization complete${NC}"

	# The kubernetes provider's config_path points to .kube/config, which only
	# exists after TKE is created (local_file.tke_kubeconfig). During the first
	# Phase 1 apply the file is missing, so the provider emits a noisy
	# "Invalid attribute in provider configuration ... no such file or directory"
	# warning even though no k8s resources are touched (count=0). Seed a minimal
	# valid placeholder kubeconfig so the path is valid; local_file.tke_kubeconfig
	# overwrites it with the real config once the cluster exists.
	if [ ! -f "${SCRIPT_DIR}/.kube/config" ]; then
		mkdir -p "${SCRIPT_DIR}/.kube"
		cat >"${SCRIPT_DIR}/.kube/config" <<-'EOF'
			apiVersion: v1
			kind: Config
			clusters: []
			contexts: []
			users: []
			current-context: ""
		EOF
		chmod 600 "${SCRIPT_DIR}/.kube/config" 2>/dev/null || true
	fi
}

# ---------------------------------------------------------------
# Step 2: terraform apply (with retry logic)
# ---------------------------------------------------------------
step2_apply() {
	local attempt=1
	local max_attempts=10
	# The transient-error branch below retries WITHOUT consuming an `attempt` (a
	# transient condition is not the operator's fault), so bound it separately:
	# otherwise a "transient" error that never clears (e.g. a jumpserver SSH that
	# never comes up, a permanently rate-limited account) would loop every 20s
	# forever — the non-interactive fail-fast sits after that `continue` and would
	# never be reached.
	local transient_attempts=0
	local max_transient="${TENCENTCLOUD_STEP_TRANSIENT_RETRIES:-15}"

	# Restrict the apply to STEP2_TARGETS when set (one phase at a time). The
	# fallback/retry logic below is unchanged; it simply applies to the targeted
	# subset (e.g. only the CVMs, or only the TKE cluster).
	local -a _tgt=()
	local _t
	for _t in "${STEP2_TARGETS[@]}"; do
		_tgt+=("-target=${_t}")
	done

	# Cross-zone fallback candidates for CVM roles (same VPC, different subnets).
	local -a _zones=()
	if [ "${STEP2_CVM_FALLBACK:-0}" = "1" ]; then
		_init_cvm_zones
		[ "${#JUMPSERVER_TYPES[@]}" -eq 0 ] && _fallback_jumpserver_instance_types
		_build_fallback_zones
		_zones=("${_FALLBACK_ZONES[@]+"${_FALLBACK_ZONES[@]}"}")
		local _ctype_count=${#CVM_TYPES[@]}
		local _jtype_count=${#JUMPSERVER_TYPES[@]}
		[ "$_ctype_count" -eq 0 ] && _ctype_count=6
		[ "$_jtype_count" -eq 0 ] && _jtype_count=5
		max_attempts=$((${#_zones[@]} * (_ctype_count + _jtype_count) + 10))
	fi

	while [ "$attempt" -le "$max_attempts" ]; do
		banner "${STEP2_LABEL} — attempt ${attempt}"
		echo -e "${YELLOW}⏳ This step provisions cloud resources and may take a long time${NC}"
		echo -e "${YELLOW}   (typically several minutes); please be patient, do not interrupt.${NC}"

		echo -e "  ${CYAN}Config:${NC}"
		echo -e "    Primary zone        : ${TF_VAR_availability_zone:-auto}"
		echo -e "    Jumpserver zone     : $(_get_role_zone jumpserver)"
		echo -e "    Compute zone        : $(_get_role_zone compute)"
		echo -e "    TKE worker zone     : $(_get_role_zone tke)"
		echo -e "    Jumpserver type     : ${TF_VAR_jumpserver_instance_type:-SA9.MEDIUM4}"
		if [ "${STEP2_COMPUTE_NODE_INDEX:-}" -ge 0 ] 2>/dev/null; then
			echo -e "    Compute preference  : $(_compute_preferred_type) (node $((STEP2_COMPUTE_NODE_INDEX + 1)) trying ${TF_VAR_compute_instance_type:-?})"
			if [ "${#COMPUTE_PURCHASED_TYPES[@]}" -gt 0 ]; then
				echo -e "    Purchased so far    : $(printf '%s ' "${COMPUTE_PURCHASED_TYPES[@]}")"
			fi
		else
			echo -e "    Compute preference  : $(_compute_preferred_type)"
			echo -e "    TKE worker type     : ${TF_VAR_tke_worker_instance_type:-SA9.LARGE8}"
		fi
		echo -e "    Operating system    : ${TF_VAR_image_name_regex:-OpenCloudOS Server 9}"
		echo ""

		if [ "${STEP2_COMPUTE_NODE_INDEX:-}" -ge 0 ] 2>/dev/null; then
			_export_compute_node_config_var "$STEP2_COMPUTE_NODE_INDEX" "${TF_VAR_compute_node_count:-2}"
			_ensure_cvm_subnet_for_zone "${TF_VAR_compute_availability_zone:-$(_compute_preferred_zone)}" || true
		fi

		# Run apply, capture the exit code
		local apply_log
		apply_log="$(mktemp "${TMPDIR:-/tmp}/tfapply_cubesandbox.XXXXXX.log")"
		local exit_code=0
		if [ "${VERBOSE}" = "1" ]; then
			terraform apply -parallelism="$TERRAFORM_PARALLELISM" -auto-approve -input=false "${_tgt[@]}" 2>&1 | tee "$apply_log" || exit_code=$?
		else
			terraform apply -parallelism="$TERRAFORM_PARALLELISM" -auto-approve -input=false "${_tgt[@]}" >"$apply_log" 2>&1 || exit_code=$?
		fi

		if [ "$exit_code" -eq 0 ]; then
			echo -e "${GREEN}✓ ${STEP2_LABEL}: complete${NC}"

			local js_ip
			js_ip=$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "N/A")

			echo ""
			echo -e "  ${YELLOW}Jumpserver public IP : ${js_ip} (ssh -p 443 root@${js_ip})${NC}"
			echo ""

			# Fetch the TKE kubeconfig early (if already deployed)
			local tke_ep_early
			tke_ep_early=$(terraform output -raw tke_cluster_endpoint 2>/dev/null || echo "")
			if [ -n "$tke_ep_early" ]; then
				echo -e "  ${CYAN}TKE API Server: ${tke_ep_early}${NC}"
				echo ""
			fi

			rm -f "$apply_log"
			return 0
		fi

		# Creation failed
		echo ""
		echo -e "${RED}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
		echo -e "${RED}  ✗ ${STEP2_LABEL} failed${NC}"
		echo -e "${RED}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
		echo ""

		# Stale-state recovery: a MySQL/Redis instance was deleted out of band, so
		# refreshing it (or its account/privilege) fails hard with "... not found".
		# Drop the affected resources from state so terraform recreates them, then
		# retry. (mysql_account/privilege error because their parent instance is gone.)
		local _stale_pruned=0 _addr
		if grep -qiE 'CdbInstanceNotFound|database instance is not found|Describe mysql ac' "$apply_log" 2>/dev/null; then
			echo -e "  ${YELLOW}MySQL instance gone (stale state); pruning dependent resources from state...${NC}"
			for _addr in tencentcloud_mysql_privilege.cube tencentcloud_mysql_account.cube null_resource.mysql_init_db tencentcloud_mysql_instance.mysql; do
				if terraform state list 2>/dev/null | grep -q "^${_addr}"; then
					echo -e "  ${CYAN}terraform state rm ${_addr}${NC}"
					terraform state rm "$_addr" >/dev/null 2>&1 || true
					_stale_pruned=1
				fi
			done
		fi
		if grep -qiE 'Fail to get info from redis|No instance found|InvalidInstanceId' "$apply_log" 2>/dev/null; then
			if terraform state list 2>/dev/null | grep -q '^tencentcloud_redis_instance.redis'; then
				echo -e "  ${YELLOW}Redis instance gone (stale state); pruning it from state...${NC}"
				terraform state rm tencentcloud_redis_instance.redis >/dev/null 2>&1 || true
				_stale_pruned=1
			fi
		fi
		if [ "$_stale_pruned" = "1" ]; then
			echo -e "  ${GREEN}-> Pruned stale resources; retrying...${NC}"
			rm -f "$apply_log"
			sleep 2
			continue
		fi

		# Extract error info. Match the various stock/out-of-resource error shapes
		# (case-insensitive), e.g. "Insufficient resource",
		# "ResourceInsufficient.SpecifiedInstanceType", "insufficient resources for
		# CVM", "sold out", "re-select the instance type". This also covers the TKE
		# cluster's inline worker_config, which uses the same instance type.
		# Match stock/availability error shapes (case-insensitive):
		#   understock : "Insufficient", "ResourceInsufficient.SpecifiedInstanceType",
		#                "sold out", "out of stock"
		#   unavailable: "ResourceUnavailable.InstanceType", "not available in the
		#                current zone" — the type is not offered in this zone (and may
		#                also block the jumpserver, whose type is not cycled).
		if grep -qiE 'insufficient|sold *out|out of stock|SpecifiedInstanceType|ResourceUnavailable|not available|re-select the instance type|InvalidZone|MismatchRegion' "$apply_log" 2>/dev/null; then
			if [ "${STEP2_CVM_FALLBACK:-0}" = "1" ]; then
				local _failed_role _zone_invalid=0
				_failed_role=$(_parse_failed_cvm_resource "$apply_log")
				# Only a genuinely unusable ZONE (retired/invalid, e.g.
				# ap-guangzhou-1 → InvalidZone.MismatchRegion) skips straight to the
				# next zone. Sold-out / "type not available in this zone" instead
				# cycle the OTHER candidate instance types in the SAME zone first,
				# and only move to another zone once every type has failed here.
				grep -qiE 'InvalidZone|MismatchRegion' "$apply_log" 2>/dev/null && _zone_invalid=1
				[ "${#CVM_TYPES[@]}" -eq 0 ] && _fallback_instance_types
				[ "${#JUMPSERVER_TYPES[@]}" -eq 0 ] && _fallback_jumpserver_instance_types
				if _try_cvm_stock_fallback "$_failed_role" "$_zone_invalid"; then
					rm -f "$apply_log"
					sleep 2
					continue
				fi
			fi
			echo -e "  ${YELLOW}Reason: all candidate instance types / availability zones have been tried or are sold out${NC}"
		elif grep -qiE 'kex_exchange_identification|Connection closed|connection refused|ZoneNotExists|解析域不存在|i/o timeout|TimeLimitExceeded|RequestLimitExceeded|try again later|waiting for one of the workers ready|workers? ready' "$apply_log" 2>/dev/null; then
			# Transient: the jumpserver SSH (443) may still be coming up (cloud-init
			# switching the port), a TCR private-zone / rate-limit hiccup may still be
			# settling, or TKE may have created the cluster but not yet reported an
			# initial worker Ready. These usually clear on their own — wait a bit and
			# retry without changing the config, but only up to max_transient times so
			# a never-clearing "transient" error eventually falls through to the
			# fail-fast / menu below instead of looping forever.
			transient_attempts=$((transient_attempts + 1))
			if [ "$transient_attempts" -lt "$max_transient" ]; then
				echo -e "  ${YELLOW}Transient error (jumpserver SSH / private zone / rate limit / TKE worker not ready); waiting 20s and retrying (${transient_attempts}/${max_transient})...${NC}"
				rm -f "$apply_log"
				sleep 20
				continue
			fi
			echo -e "  ${YELLOW}Transient error persisted after ${max_transient} retries; no longer treating it as transient.${NC}"
		else
			echo -e "  ${YELLOW}See the log above for error details${NC}"
		fi

		# No interactive terminal (CI / piped stdin): there is nobody to answer the
		# menu below. read would hit EOF on every iteration, leaving $action empty
		# and spinning forever on the "Invalid input" branch. Fail fast instead.
		if [ ! -t 0 ]; then
			if [ "${STEP2_CVM_FALLBACK:-0}" = "1" ]; then
				echo -e "${RED}✗ CVM provisioning failed and stdin is not a terminal — cannot prompt for a retry option.${NC}"
				echo -e "  ${YELLOW}Review the error above (full log: ${apply_log}); adjust the instance type / zone via env vars and re-run.${NC}"
			else
				echo -e "${RED}✗ ${STEP2_LABEL}: failed and stdin is not a terminal — cannot prompt for a retry option.${NC}"
				echo -e "  ${YELLOW}Review the error above (full log: ${apply_log}) and re-run create.sh.${NC}"
			fi
			rm -f "$apply_log"
			exit 1
		fi

		# Non-CVM apply (e.g. the TKE addons): the CVM zone/instance-type menu
		# below is irrelevant, so offer a simple retry/exit instead of prompting
		# the operator to re-pick a CVM spec for a Kubernetes failure.
		if [ "${STEP2_CVM_FALLBACK:-0}" != "1" ]; then
			echo ""
			echo -e "${CYAN}Please choose:${NC}"
			echo -e "  ${GREEN}1)${NC} Retry this step"
			echo -e "  ${GREEN}2)${NC} Exit"
			echo ""
			local _na_action
			while true; do
				read -r -p "$(echo -e "${YELLOW}Enter [1-2]: ${NC}")" _na_action
				case "$_na_action" in
				1)
					echo -e "${CYAN}Retrying...${NC}"
					sleep 2
					break
					;;
				2)
					echo -e "${RED}Cancelled by user, exiting${NC}"
					rm -f "$apply_log"
					exit 1
					;;
				*) echo -e "${RED}Invalid input${NC}" ;;
				esac
			done
			rm -f "$apply_log"
			attempt=$((attempt + 1))
			continue
		fi

		echo ""
		echo -e "${CYAN}Please choose:${NC}"
		echo -e "  ${GREEN}1)${NC} Re-select availability zone and instance type"
		echo -e "  ${GREEN}2)${NC} Keep the current config and retry immediately"
		echo -e "  ${GREEN}3)${NC} Enter a new instance type manually, then retry"
		echo -e "  ${GREEN}4)${NC} Exit"
		echo ""

		local action
		while true; do
			read -r -p "$(echo -e "${YELLOW}Enter [1-4]: ${NC}")" action
			case "$action" in
			1)
				# Reset the selection and go through the interactive flow again
				unset TF_VAR_availability_zone
				unset TF_VAR_jumpserver_availability_zone
				unset TF_VAR_compute_availability_zone
				unset TF_VAR_tke_worker_availability_zone
				unset TF_VAR_compute_instance_type
				CVM_TYPE_INDEX=-1
				JUMPSERVER_TYPE_INDEX=-1
				JUMPSERVER_ZONE_INDEX=0
				COMPUTE_ZONE_INDEX=0
				TKE_ZONE_INDEX=0
				select_zone
				select_instance_type
				_init_cvm_zones
				break
				;;
			2)
				echo -e "${CYAN}Keeping the current config, retrying immediately...${NC}"
				sleep 2
				break
				;;
			3)
				local new_type
				read -r -p "$(echo -e "${YELLOW}Enter an instance type: ${NC}")" new_type
				if [ -n "$new_type" ]; then
					export TF_VAR_compute_instance_type="$new_type"
					CVM_TYPE_INDEX=-1
					echo -e "${GREEN}✓ Instance type updated: ${new_type}${NC}"
				fi
				break
				;;
			4)
				echo -e "${RED}Cancelled by user, exiting${NC}"
				rm -f "$apply_log"
				exit 1
				;;
			*)
				echo -e "${RED}Invalid input${NC}"
				;;
			esac
		done

		rm -f "$apply_log"
		attempt=$((attempt + 1))
	done

	echo -e "${RED}Maximum retries reached (${max_attempts}), please retry manually later${NC}"
	exit 1
}

# ---------------------------------------------------------------
# _apply_phase — run a single, synchronous terraform apply phase and wait for it
#   to finish. Restricts the apply to the given resource addresses via -target
#   (pass no addresses for a full apply). Returns 0 on success and non-zero on
#   failure (printing the tail of the log), so the orchestrator can fail-fast and
#   stop the deployment instead of pressing on to the next phase.
#   Usage: _apply_phase "<banner label>" [resource.addr ...]
# ---------------------------------------------------------------
_apply_phase() {
	local label="$1"
	shift
	local -a _tgt=()
	local _t
	for _t in "$@"; do
		_tgt+=("-target=${_t}")
	done

	banner "${label}"
	echo -e "${YELLOW}⏳ This step provisions cloud resources and may take a long time${NC}"
	echo -e "${YELLOW}   (typically several minutes); please be patient, do not interrupt.${NC}"

	local log
	log="$(mktemp "${TMPDIR:-/tmp}/tf_phase_apply.XXXXXX.log")"
	local rc=0
	if [ "${VERBOSE}" = "1" ]; then
		terraform apply -parallelism="$TERRAFORM_PARALLELISM" -auto-approve -input=false "${_tgt[@]}" 2>&1 | tee "$log" || rc=$?
	else
		terraform apply -parallelism="$TERRAFORM_PARALLELISM" -auto-approve -input=false "${_tgt[@]}" >"$log" 2>&1 || rc=$?
	fi

	if [ "$rc" -ne 0 ]; then
		echo -e "${RED}✗ ${label}: failed${NC}"
		echo -e "  ${YELLOW}Last error (full log: ${log}):${NC}"
		tail -n 20 "$log" 2>/dev/null | sed 's/^/    /'
		return 1
	fi
	rm -f "$log"
	echo -e "${GREEN}✓ ${label}: complete${NC}"
	return 0
}

# ---------------------------------------------------------------
# Jumpserver SSH execution helpers
# ---------------------------------------------------------------

# Execute a command on the jumpserver (with timeout protection)
_jump_exec() {
	local cmd="$1"
	local js_pub_ip
	js_pub_ip=$(_js_pub_ip)
	[ -z "$js_pub_ip" ] && {
		echo "  (jumpserver unavailable)"
		return 1
	}
	local key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"
	ssh -i "${key_file}" -p 443 \
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 \
		-o BatchMode=yes -o LogLevel=ERROR \
		root@"${js_pub_ip}" "${cmd}" 2>&1 || true
}

# Like _jump_exec, but feeds $1 to the remote command's stdin. Use it for
# secrets (e.g. a DB password): the remote command reads them with $(cat) and
# exports them as an env var, so the secret never appears on the remote process
# argv / `ps` output (CWE-214). $2 is the remote command.
_jump_exec_stdin() {
	local stdin_payload="$1" cmd="$2"
	local js_pub_ip
	js_pub_ip=$(_js_pub_ip)
	[ -z "$js_pub_ip" ] && {
		echo "  (jumpserver unavailable)"
		return 1
	}
	local key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"
	printf '%s' "${stdin_payload}" | ssh -i "${key_file}" -p 443 \
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o ConnectTimeout=5 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 \
		-o BatchMode=yes -o LogLevel=ERROR \
		root@"${js_pub_ip}" "${cmd}" 2>&1 || true
}

# ---------------------------------------------------------------
# _deploy_mysql — get MySQL instance info
# ---------------------------------------------------------------
_deploy_mysql() {
	local mysql_ip
	mysql_ip=$(terraform output -raw mysql_intranet_ip 2>/dev/null || echo "")

	if [ -z "$mysql_ip" ]; then
		echo -e "  ${CYAN}MySQL: not created${NC}"
		return 1
	fi

	local mysql_port
	mysql_port=$(terraform output -raw mysql_intranet_port 2>/dev/null || echo "3306")

	echo -e "  ${GREEN}MySQL instance: ${mysql_ip}:${mysql_port}${NC}"
	return 0
}

# ---------------------------------------------------------------
_deploy_redis() {
	local redis_ip
	redis_ip=$(terraform output -raw redis_intranet_ip 2>/dev/null || echo "")

	if [ -z "$redis_ip" ]; then
		echo -e "  ${CYAN}Redis: not created${NC}"
		return 1
	fi

	local redis_port
	redis_port=$(terraform output -raw redis_intranet_port 2>/dev/null || echo "6379")

	echo -e "  ${GREEN}Redis instance: ${redis_ip}:${redis_port}${NC}"
	return 0
}

# ---------------------------------------------------------------
# _init_redis — initialize Redis (install redis-cli + verify connectivity)
# ---------------------------------------------------------------
_init_redis() {
	local redis_ip
	redis_ip=$(terraform output -raw redis_intranet_ip 2>/dev/null || echo "")

	if [ -z "$redis_ip" ]; then
		echo -e "  ${CYAN}No Redis instance detected, skipping initialization${NC}"
		return 0
	fi

	banner "Initialize external Redis (via jumpserver)"

	local redis_port redis_password
	local js_public_ip key_file

	redis_port=$(terraform output -raw redis_intranet_port 2>/dev/null || echo "6379")
	# Use the SAME password the Redis instance was actually created with. setup_env
	# exports TF_VAR_redis_password from TENCENTCLOUD_REDIS_PASSWORD, but prefer
	# TF_VAR_redis_password directly so a raw `TF_VAR_redis_password=...` run (no
	# TENCENTCLOUD_* var) still PINGs with the right password instead of falling
	# back to the demo default and failing an otherwise-healthy deployment.
	redis_password="${TF_VAR_redis_password:-${TENCENTCLOUD_REDIS_PASSWORD:-ceuhvu123}}"
	js_public_ip=$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "")
	key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"

	# SSH directly to jumpserver:443
	local js_ssh=(
		ssh -i "${key_file}" -p 443
		-o StrictHostKeyChecking=no
		-o UserKnownHostsFile=/dev/null
		-o ConnectTimeout=10
		-o BatchMode=yes
		-o LogLevel=ERROR
	)

	echo -e "  ${CYAN}jumpserver: ${js_public_ip}:443${NC}"
	echo -e "  ${CYAN}Redis: ${redis_ip}:${redis_port}${NC}"
	echo ""

	# ---- Install redis-cli ----
	echo -e "  ${CYAN}[1/2] Installing redis-cli...${NC}"
	"${js_ssh[@]}" root@"${js_public_ip}" "command -v redis-cli &>/dev/null || sudo dnf install -y redis" 2>&1 || {
		echo -e "  ${RED}✗ redis-cli installation failed${NC}"
		exit 1
	}
	echo -e "  ${GREEN}✓ redis-cli ready${NC}"

	# ---- Verify Redis is reachable ----
	echo ""
	echo -e "  ${CYAN}[2/2] Verifying Redis is reachable...${NC}"

	local redis_ping_out
	# Feed the password over stdin (read back by $(cat) on the remote) so it lands
	# in neither the LOCAL ssh argv nor the remote redis-cli argv — both are
	# world-readable via `ps` (CWE-214). REDISCLI_AUTH then keeps it out of the
	# remote argv. Mirrors the MySQL/tccli stdin pattern used elsewhere.
	redis_ping_out=$(printf '%s' "${redis_password}" | "${js_ssh[@]}" root@"${js_public_ip}" \
		"set +H; REDISCLI_AUTH=\"\$(cat)\" redis-cli -h '${redis_ip}' -p '${redis_port}' --no-auth-warning PING 2>&1" 2>&1) || true

	if echo "$redis_ping_out" | grep -q "PONG"; then
		echo -e "  ${GREEN}✓ Redis reachable (requirepass enabled)${NC}"
	else
		echo -e "  ${RED}✗ Redis verification failed: ${redis_ping_out}${NC}"
		exit 1
	fi

	echo ""
	echo -e "  ${GREEN}✓ Redis initialization complete${NC}"
	echo -e "    Address: ${redis_ip}:${redis_port}"
	echo ""
}

# Step 6: Replace with the CubeSandbox kernel
# ---------------------------------------------------------------
step6_replace_kernel() {
	# Optional target_ip (compute node), defaults to the control node
	local target_ip="${1:-}"
	local label="${2:-CVM}"
	# Callers (step8_init_compute_nodes) always pass an explicit compute IP. There
	# is no "private_ip" terraform output to fall back on, so refuse to proceed
	# with an empty target instead of attempting to ssh to "root@" (empty host).
	if [ -z "$target_ip" ]; then
		echo -e "${RED}✗ step6_replace_kernel: no target IP provided${NC}" >&2
		return 1
	fi

	banner "Step: Replace ${label} with the CubeSandbox PVM kernel"

	local key_file js_public_ip
	key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"
	js_public_ip=$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "")
	local pvm_kernel_url="${TENCENTCLOUD_PVM_KERNEL_RPM_URL:-https://mirrors.opencloudos.tech/opencloudos/9.6/extras/x86_64/os/Packages/kernel-core-6.6.69-1.2.cubesandbox.oc9.x86_64.rpm}"
	local rpm_name
	rpm_name=$(basename "$pvm_kernel_url")
	local rpm_file="/tmp/${rpm_name}"

	# SSH to the target node (through the jumpserver proxy)
	local ssh_opts=(
		-i "${key_file}"
		-p "${SSH_PORT}"
		-o StrictHostKeyChecking=no
		-o UserKnownHostsFile=/dev/null
		-o ConnectTimeout=10
		-o BatchMode=yes
		-o LogLevel=ERROR
		"${JUMP_PROXY_OPTS[@]}"
	)

	# Pre-check
	if [ "${REINSTALL:-0}" != "1" ] && [ "${REINSTALL:-0}" != "true" ]; then
		local kvm_check
		kvm_check=$(ssh "${ssh_opts[@]}" root@"${target_ip}" "ls -la /dev/kvm 2>&1 && modinfo kvm_pvm 2>&1" 2>&1) || true
		if echo "$kvm_check" | grep -q "kvm_pvm"; then
			echo -e "  ${GREEN}✓ /dev/kvm + kvm_pvm ready; the ${label} PVM kernel is already installed, skipping replacement${NC}"
			echo -e "  ${CYAN}  (set TENCENTCLOUD_REINSTALL=1 to force a reinstall)${NC}"
			return 0
		fi
	fi

	# 1) Download the RPM on the jumpserver (reusing a previously cached copy).
	#    Reuse the cache only when the file is NON-EMPTY ([ -s ]): a prior failed
	#    `wget -O` leaves a 0-byte file that would otherwise be treated as cached
	#    and distributed/installed as a broken RPM on every rerun. On download
	#    failure, remove the partial file (no poisoned cache) and surface FAILED so
	#    the marker check below actually aborts (the old `|| echo FAILED` made the
	#    remote command exit 0, so a failed download was treated as success).
	echo -e "  ${CYAN}[1/5] Downloading the PVM kernel RPM on the jumpserver...${NC}"
	echo -e "  URL: ${pvm_kernel_url}"
	local _rpm_dl_out
	_rpm_dl_out=$(ssh -i "${key_file}" -p 443 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o BatchMode=yes -o LogLevel=ERROR root@"${js_public_ip}" "
    if [ -s ${rpm_file} ]; then
      echo 'CACHED'
    elif wget -q --timeout=30 --tries=3 '${pvm_kernel_url}' -O ${rpm_file} && [ -s ${rpm_file} ]; then
      echo 'DOWNLOADED'
    else
      rm -f ${rpm_file}
      echo 'FAILED'
    fi
  " 2>&1) || true
	if ! printf '%s' "$_rpm_dl_out" | grep -qE 'DOWNLOADED|CACHED'; then
		echo ""
		echo -e "${RED}✗ RPM download failed${NC}"
		printf '%s\n' "$_rpm_dl_out" | sed 's/^/    /'
		return 1
	fi
	echo -e "${GREEN}✓ RPM ready (jumpserver:${rpm_file})${NC}"

	# 2) Distribute the RPM from the jumpserver to the target node
	echo ""
	echo -e "  ${CYAN}[2/5] Distributing the RPM from the jumpserver to ${label} (${target_ip})...${NC}"
	ssh -i "${key_file}" -p 443 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o BatchMode=yes -o LogLevel=ERROR root@"${js_public_ip}" "
    scp -i /root/.ssh/id_rsa -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 ${rpm_file} root@${target_ip}:${rpm_file}
  " 2>&1 || {
		echo -e "${RED}✗ RPM distribution failed${NC}"
		return 1
	}
	echo -e "${GREEN}✓ RPM distributed to ${label}${NC}"

	# 3) Install the RPM
	echo ""
	echo -e "  ${CYAN}[3/5] Installing the PVM kernel RPM...${NC}"

	local rpm_basename already_installed
	rpm_basename="${rpm_name%.rpm}"
	already_installed=$(ssh "${ssh_opts[@]}" root@"${target_ip}" "rpm -q '${rpm_basename}' 2>&1" || true)
	if echo "$already_installed" | grep -q "^${rpm_basename}"; then
		echo -e "${GREEN}✓ Kernel RPM already installed, skipping${NC}"
	else
		local install_ok=0
		local install_out install_exit

		_is_install_success() {
			local out="$1"
			local filtered
			filtered=$(echo "$out" | grep -v "dracut-install:" || true)
			echo "$filtered" | grep -qE "(#+.*#+|Verifying|installed|already)"
		}

		echo -e "  ${CYAN}Trying: rpm -ivh --oldpackage --nodeps${NC}"
		install_out=$(ssh "${ssh_opts[@]}" root@"${target_ip}" \
			"sudo rpm -ivh --oldpackage --nodeps --nosignature '${rpm_file}' 2>&1; echo EXIT:\$?") || true
		echo "$install_out" | grep -v "^EXIT:" || true
		install_exit=$(echo "$install_out" | grep "^EXIT:" | head -1 | cut -d: -f2 || echo "1")
		[ "$install_exit" = "0" ] || _is_install_success "$install_out" && install_ok=1

		if [ "$install_ok" -eq 0 ]; then
			echo ""
			echo -e "  ${YELLOW}Trying dnf install...${NC}"
			install_out=$(ssh "${ssh_opts[@]}" root@"${target_ip}" \
				"sudo dnf install -y --nogpgcheck '${rpm_file}' 2>&1; echo EXIT:\$?") || true
			echo "$install_out" | grep -v "^EXIT:" || true
			install_exit=$(echo "$install_out" | grep "^EXIT:" | head -1 | cut -d: -f2 || echo "1")
			[ "$install_exit" = "0" ] || echo "$install_out" | grep -qiE "(Complete|Nothing to do|already|Installed)" && install_ok=1
		fi

		if [ "$install_ok" -eq 0 ]; then
			echo ""
			echo -e "  ${YELLOW}Trying rpm --force...${NC}"
			install_out=$(ssh "${ssh_opts[@]}" root@"${target_ip}" \
				"sudo rpm -ivh --force --nodeps --nosignature '${rpm_file}' 2>&1; echo EXIT:\$?") || true
			echo "$install_out" | grep -v "^EXIT:" || true
			install_exit=$(echo "$install_out" | grep "^EXIT:" | head -1 | cut -d: -f2 || echo "1")
			[ "$install_exit" = "0" ] || _is_install_success "$install_out" && install_ok=1
		fi

		if [ "$install_ok" -eq 0 ]; then
			echo ""
			echo -e "${RED}✗ RPM installation failed${NC}"
			echo "$install_out" | grep -v "^EXIT:" | tail -10
			return 1
		fi
	fi
	echo ""
	echo -e "${GREEN}✓ RPM installation complete${NC}"

	# 4) Update the cmdline
	echo ""
	echo -e "  ${CYAN}[4/5] Updating the kernel cmdline...${NC}"

	local grub_cn="https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/pvm/grub/host_grub_config.sh"
	local grub_gh="https://raw.githubusercontent.com/TencentCloud/CubeSandbox/master/deploy/pvm/grub/host_grub_config.sh"

	local grub_out
	grub_out=$(ssh "${ssh_opts[@]}" -o ConnectTimeout=10 root@"${target_ip}" \
		"(curl -fsSL --connect-timeout 10 --max-time 30 '${grub_cn}' 2>/dev/null || \
      curl -fsSL --connect-timeout 10 --max-time 30 '${grub_gh}') | bash 2>&1" 2>&1) || true

	echo "$grub_out"
	echo ""

	if [ -z "$grub_out" ]; then
		echo -e "  ${RED}✗ cmdline update produced no output${NC}"
	elif echo "$grub_out" | grep -qiE "(error|failed|fatal|command not found)"; then
		echo -e "  ${RED}✗ cmdline update may have failed${NC}"
	else
		echo -e "  ${GREEN}✓ cmdline update complete${NC}"
	fi

	# 5) Set the default kernel
	echo ""
	echo -e "  ${CYAN}[5/5] Setting CubeSandbox as the default boot entry...${NC}"

	local set_default_out
	set_default_out=$(ssh "${ssh_opts[@]}" root@"${target_ip}" '
    set -e
    CUBE_KERNEL=$(ls /boot/vmlinuz-*cubesandbox* 2>/dev/null | head -1)
    if [ -z "$CUBE_KERNEL" ]; then
      echo "ERROR: cubesandbox kernel file not found"
      exit 1
    fi
    echo "Found kernel: $CUBE_KERNEL"

    if command -v grubby &>/dev/null; then
      grubby --set-default="$CUBE_KERNEL"
      echo "grubby default kernel: $(grubby --default-kernel)"
    elif command -v grub2-set-default &>/dev/null && [ -f /boot/grub2/grub.cfg ]; then
      ENTRY=$(awk -F"'\''" '\''/menuentry.*cubesandbox/ {print $2; exit}'\'' /boot/grub2/grub.cfg)
      if [ -n "$ENTRY" ]; then
        grub2-set-default "$ENTRY"
        echo "grub2 default entry: $ENTRY"
      fi
    else
      KERNEL_VER=$(basename "$CUBE_KERNEL" | sed "s/^vmlinuz-//")
      if [ -n "$KERNEL_VER" ] && [ -f /etc/default/grub ]; then
        sed -i "s/^GRUB_DEFAULT=.*/GRUB_DEFAULT=\"Advanced options for OpenCloudOS>OpenCloudOS ($KERNEL_VER) $(uname -m)\"/" /etc/default/grub
        [ -x /usr/sbin/grub2-mkconfig ] && grub2-mkconfig -o /boot/grub2/grub.cfg 2>/dev/null || true
        echo "Set via /etc/default/grub: $KERNEL_VER"
      fi
    fi
  ' 2>&1) || true

	echo "$set_default_out"

	if echo "$set_default_out" | grep -q "^ERROR"; then
		echo ""
		echo -e "${RED}✗ Failed to set the default boot entry${NC}"
		return 1
	fi
	echo ""
	echo -e "${GREEN}✓ CubeSandbox set as the default boot entry${NC}"

	echo ""
	echo -e "${GREEN}✓ CubeSandbox PVM kernel replacement complete, ready to reboot${NC}"
}

# ---------------------------------------------------------------
# Step 7: Reboot the CVM and verify the new kernel
# ---------------------------------------------------------------
step7_reboot_and_verify() {
	# Optional public_ip (compute node), defaults to the control node
	local public_ip="${1:-}"
	local label="${2:-CVM}"
	local skip_install="${3:-}" # if non-empty, skip CubeSandbox installation (kernel verification only)
	# Callers always pass an explicit compute IP; there is no "private_ip" output
	# to fall back on, so refuse to proceed with an empty target.
	if [ -z "$public_ip" ]; then
		echo -e "${RED}✗ step7_reboot_and_verify: no target IP provided${NC}" >&2
		return 1
	fi

	banner "Step: ${label} — reboot + verify kernel + install CubeSandbox"

	local key_file
	key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"

	local ssh_opts=(
		-i "${key_file}"
		-p "${SSH_PORT}"
		-o StrictHostKeyChecking=no
		-o UserKnownHostsFile=/dev/null
		-o ConnectTimeout=5
		-o BatchMode=yes
		-o LogLevel=ERROR

		"${JUMP_PROXY_OPTS[@]}"
	)

	# Pre-check: if /dev/kvm exists and the kvm_pvm module is available, skip the reboot
	if [ "${REINSTALL:-0}" != "1" ] && [ "${REINSTALL:-0}" != "true" ]; then
		local kvm_check
		kvm_check=$(ssh "${ssh_opts[@]}" root@"${public_ip}" "ls -la /dev/kvm 2>&1 && modinfo kvm_pvm 2>&1" 2>&1) || true
		if echo "$kvm_check" | grep -q "kvm_pvm"; then
			echo -e "  ${GREEN}✓ /dev/kvm + kvm_pvm ready; the ${label} PVM kernel is already ready, skipping reboot${NC}"
			echo ""
			# Jump directly to the install check section
			local has_kvm="/dev/kvm"
		fi
	fi

	if [ -z "${has_kvm:-}" ]; then
		# Snapshot the boot id BEFORE rebooting. sshd usually stays up for several
		# seconds after `reboot`, so a bare "is SSH up?" check can succeed against
		# the still-running pre-reboot system and wrongly report "online" before
		# the reboot even happened. Waiting for the boot id to CHANGE proves the
		# host actually came back up on the new boot. If it can't be read, fall
		# back to the old SSH-answered check.
		local pre_boot_id
		pre_boot_id=$(ssh "${ssh_opts[@]}" root@"${public_ip}" "cat /proc/sys/kernel/random/boot_id" 2>/dev/null | tr -d '[:space:]') || true

		# 1) Perform the reboot
		echo -e "  ${CYAN}Rebooting the CVM...${NC}"
		ssh "${ssh_opts[@]}" root@"${public_ip}" "sudo reboot" 2>&1 || true
		sleep 10

		# 2) Wait for SSH to recover (and, when known, the boot id to change)
		echo -e "  ${CYAN}Waiting for the CVM reboot to finish...${NC}"
		echo -n "  "

		local ssh_ok=0 i cur_boot_id
		for i in $(seq 1 30); do
			cur_boot_id=$(ssh "${ssh_opts[@]}" root@"${public_ip}" "cat /proc/sys/kernel/random/boot_id" 2>/dev/null | tr -d '[:space:]') || true
			if [ -n "$cur_boot_id" ]; then
				# No pre-reboot id to compare against → SSH answering is the best
				# signal we have. Otherwise require the boot id to differ.
				if [ -z "$pre_boot_id" ] || [ "$cur_boot_id" != "$pre_boot_id" ]; then
					ssh_ok=1
					break
				fi
			fi
			echo -n "."
			sleep 5
		done

		echo ""

		if [ "$ssh_ok" -ne 1 ]; then
			echo -e "${RED}✗ SSH connection timed out after the CVM reboot${NC}"
			echo -e "  ${YELLOW}Please check manually: ssh -i ${key_file} -p ${SSH_PORT} -J root@<jumpserver>:443 root@${public_ip}${NC}"
			return 1
		fi

		echo -e "${GREEN}✓ CVM is back online${NC}"
		echo ""

		# Configure the kvm_pvm module to auto-load at boot
		echo -e "  ${CYAN}Configuring kvm_pvm to auto-load at boot...${NC}"
		ssh "${ssh_opts[@]}" root@"${public_ip}" \
			"echo 'kvm_pvm' | sudo tee /etc/modules-load.d/kvm-pvm.conf" 2>&1 || true
		echo -e "  ${GREEN}✓ kvm_pvm configured to auto-load${NC}"

		# 3) Verify the new kernel
		echo -e "  ${CYAN}[Verify 1/5] New kernel version...${NC}"
		echo ""

		local new_kernel
		new_kernel=$(ssh "${ssh_opts[@]}" root@"${public_ip}" "uname -r" 2>&1) || true
		new_kernel=$(echo "$new_kernel" | tr -d '\r')

		echo -e "  ${YELLOW}New kernel version: ${new_kernel}${NC}"

		if echo "$new_kernel" | grep -qi "cubesandbox"; then
			echo ""
			_draw_box "${GREEN}" "Kernel replacement succeeded! CubeSandbox PVM kernel"
		else
			echo ""
			_draw_box "${RED}" "Kernel verification failed! No cubesandbox marker"
			# The node booted the wrong kernel (grub default / cmdline did not take).
			# Fail now: proceeding would only resurface as a confusing "/dev/kvm
			# missing" / compute-install failure later. The caller records this node
			# as failed; re-run create.sh (optionally TENCENTCLOUD_REINSTALL=1) to retry.
			echo -e "  ${YELLOW}Node did not boot the CubeSandbox PVM kernel (uname -r: ${new_kernel:-unknown}); aborting this node.${NC}"
			return 1
		fi

		# 4) Inspect /proc/cmdline
		echo ""
		echo -e "  ${CYAN}[Verify 2/5] Kernel boot parameters (cmdline)...${NC}"
		local cmdline
		cmdline=$(ssh "${ssh_opts[@]}" root@"${public_ip}" "cat /proc/cmdline" 2>&1) || true
		echo -e "  ${YELLOW}${cmdline}${NC}"

	fi # end of reboot block (skipped if /dev/kvm already existed)

	# 5) Try to load the kvm_pvm module and check /dev/kvm
	echo ""
	echo -e "  ${CYAN}[Verify 3/5] Load kvm_pvm and install CubeSandbox...${NC}"

	# First try modprobe kvm_pvm
	echo -e "  ${CYAN}Trying modprobe kvm_pvm...${NC}"
	local modprobe_out
	modprobe_out=$(ssh "${ssh_opts[@]}" root@"${public_ip}" "sudo modprobe kvm_pvm 2>&1" 2>&1) || true
	echo "  $modprobe_out"

	# Check /dev/kvm
	local has_kvm
	has_kvm=$(ssh "${ssh_opts[@]}" root@"${public_ip}" "ls -la /dev/kvm 2>&1" 2>&1) || true

	if [ -n "$skip_install" ]; then
		echo -e "  ${CYAN}Skipping CubeSandbox installation (compute nodes are installed by step8)${NC}"
	elif echo "$has_kvm" | grep -q "/dev/kvm"; then
		echo -e "  ${GREEN}✓ /dev/kvm exists${NC}"
	else
		echo -e "  ${YELLOW}⚠ /dev/kvm does not exist, skipping CubeSandbox installation${NC}"
		echo -e "  ${YELLOW}  (the CVM does not support nested virtualization, cannot install CubeSandbox)${NC}"
	fi

	echo -e "  ${CYAN}Full system info (uname -a):${NC}"
	ssh "${ssh_opts[@]}" root@"${public_ip}" "uname -a" 2>&1 || true
}

# ---------------------------------------------------------------
# Step 8: Initialize all compute nodes (purchased sequentially in step 3)
# ---------------------------------------------------------------
step8_init_compute_nodes() {
	_setup_jump_proxy
	local key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"

	# Get the cube-master CLB IP (VPC internal address)
	local cm_clb_ip
	cm_clb_ip=$(terraform output -raw tke_cubemaster_clb_ip 2>/dev/null || echo "")

	# cube-egress image mirror for the compute nodes. This terraform deployer
	# always runs inside Tencent Cloud, so the China-region pull-through
	# (cube-sandbox-cn.tencentcloudcr.com, selected by MIRROR=cn in
	# cube-egress-start.sh) is the right source; the image is published to both
	# the cn and int registries at the same digest, and a per-node override is
	# still possible via CUBE_SANDBOX_CUBE_EGRESS_IMAGE. Both compute install
	# paths below pass this same value.
	local egress_mirror="cn"

	# Read the compute node outputs once: they do not change during this function,
	# so caching avoids re-spawning `terraform output` (which parses the whole
	# state) 2-3x per node inside the loops below.
	local compute_count compute_ips_json compute_ids_json compute_types_json i
	compute_ips_json=$(terraform output -json compute_private_ips 2>/dev/null || echo "[]")
	compute_ids_json=$(terraform output -json compute_instance_ids 2>/dev/null || echo "[]")
	compute_types_json=$(terraform output -json compute_instance_types 2>/dev/null || echo "[]")
	compute_count=$(printf '%s' "$compute_ips_json" | jq -r 'length' 2>/dev/null || echo "0")

	if [ "$compute_count" -eq 0 ]; then
		return 0
	fi

	# Pre-check: if the bundle is not updated and REINSTALL=0 and RESET_DB=0, and `cubecli ls` works on all compute nodes, skip entirely
	if [ "${BUNDLE_UPDATED:-0}" != "1" ] && [ "${REINSTALL:-0}" != "1" ] && [ "${REINSTALL:-0}" != "true" ] && [ "${RESET_DB:-0}" != "1" ] && [ "${RESET_DB:-0}" != "true" ]; then
		local all_ok=1
		for ((ci = 0; ci < compute_count; ci++)); do
			local cpo
			cpo=$(printf '%s' "$compute_ips_json" | jq -r ".[$ci]" 2>/dev/null || echo "")
			local pf_out
			# Compute nodes only have private IPs, so the pre-check must go
			# through the jumpserver ProxyCommand (same as the ssh_opts used
			# for the real install below). Without it this always fails from
			# outside the VPC and the "already installed → skip" path never
			# triggers, forcing a full kernel reinstall on every re-run.
			pf_out=$(ssh -i "${key_file}" -p "${SSH_PORT}" \
				-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
				-o ConnectTimeout=5 -o BatchMode=yes -o LogLevel=ERROR \
				"${JUMP_PROXY_OPTS[@]}" \
				root@"${cpo}" "cubecli ls 2>&1" 2>&1) || true
			if echo "$pf_out" | grep -qiE "(NAME|READY|STATUS|sandbox)"; then
				echo -e "  ${GREEN}✓ Compute node ${cpo} cubecli ls works${NC}"
			else
				all_ok=0
				break
			fi
		done
		if [ "$all_ok" -eq 1 ]; then
			echo -e "  ${GREEN}✓ CubeSandbox is installed on all compute nodes, skipping initialization${NC}"
			echo -e "  ${CYAN}  (set TENCENTCLOUD_REINSTALL=1 to force a reinstall)${NC}"
			return 0
		fi
	fi

	banner "Step: Initialize ${compute_count} compute node(s)"

	# Ensure a PVM guest kernel (vmlinux-pvm) is available before installing: if
	# the bundle does not ship one, ask the user for a local file or a web URL.
	resolve_vmlinux_pvm

	local ssh_opts=(
		-i "${key_file}"
		-p "${SSH_PORT}"
		-o StrictHostKeyChecking=no
		-o UserKnownHostsFile=/dev/null
		-o ConnectTimeout=5
		-o BatchMode=yes
		-o LogLevel=ERROR

		"${JUMP_PROXY_OPTS[@]}"
	)

	# Track per-node init failures so the deployment fails fast instead of
	# printing "deployment complete" with a cluster that is missing compute
	# capacity.
	local failed_nodes=()

	for ((i = 0; i < compute_count; i++)); do
		local node_num=$((i + 1))
		local compute_private_ip compute_id compute_type
		compute_private_ip=$(printf '%s' "$compute_ips_json" | jq -r ".[$i]" 2>/dev/null || echo "")
		compute_id=$(printf '%s' "$compute_ids_json" | jq -r ".[$i]" 2>/dev/null || echo "")
		compute_type=$(printf '%s' "$compute_types_json" | jq -r ".[$i]" 2>/dev/null || echo "")

		echo ""
		echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
		echo -e "${CYAN}  Compute node ${node_num}/${compute_count} (${compute_type:-unknown}): ${compute_private_ip}${NC}"
		echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
		echo ""

		# 1) Replace the kernel
		echo -e "  ${CYAN}[${node_num}.1] Replacing the kernel...${NC}"
		step6_replace_kernel "$compute_private_ip" "compute node ${node_num}" || {
			echo -e "  ${RED}✗ Kernel replacement failed on compute node ${node_num}, skipping${NC}"
			failed_nodes+=("${compute_private_ip} (kernel replace)")
			continue
		}

		# 2) Reboot + verify + kvm_pvm
		echo -e "  ${CYAN}[${node_num}.2] Reboot + verify...${NC}"
		step7_reboot_and_verify "$compute_private_ip" "compute node ${node_num}" "skip" || {
			echo -e "  ${RED}✗ Reboot verification failed on compute node ${node_num}, skipping${NC}"
			failed_nodes+=("${compute_private_ip} (reboot/verify)")
			continue
		}

		# 3) Install CubeSandbox (compute role)
		echo ""
		echo -e "  ${CYAN}[${node_num}.3] Installing CubeSandbox compute...${NC}"
		echo -e "  ${CYAN}Control plane IP (cube-master CLB): ${cm_clb_ip}${NC}"
		echo -e "  ${CYAN}Local internal IP: ${compute_private_ip}${NC}"
		echo ""

		# Pre-check: if `cubecli ls` works, CubeSandbox compute is already installed
		# But if the bundle has been updated or RESET_DB=1, it needs to be redistributed and reinstalled
		if [ "${BUNDLE_UPDATED:-0}" != "1" ] && [ "${REINSTALL:-0}" != "1" ] && [ "${REINSTALL:-0}" != "true" ] && [ "${RESET_DB:-0}" != "1" ] && [ "${RESET_DB:-0}" != "true" ]; then
			local preflight_out
			preflight_out=$(ssh "${ssh_opts[@]}" root@"${compute_private_ip}" "cubecli ls 2>&1" 2>&1) || true
			if echo "$preflight_out" | grep -qiE "(NAME|READY|STATUS|sandbox)"; then
				echo -e "  ${GREEN}✓ cubecli ls works, re-registering with cube-master...${NC}"
				ssh "${ssh_opts[@]}" root@"${compute_private_ip}" "sh /usr/local/services/cubetoolbox/scripts/one-click/down-compute.sh 2>&1" 2>&1 || true
				sleep 3
				ssh "${ssh_opts[@]}" root@"${compute_private_ip}" "sed -i 's/^ONE_CLICK_CONTROL_PLANE_IP=.*/ONE_CLICK_CONTROL_PLANE_IP=\"${cm_clb_ip}\"/' /usr/local/services/cubetoolbox/.one-click.env 2>&1" 2>&1 || true
				ssh "${ssh_opts[@]}" root@"${compute_private_ip}" "sh /usr/local/services/cubetoolbox/scripts/one-click/up-compute.sh 2>&1" 2>&1 || true
				echo -e "  ${GREEN}✓ Compute node re-registered${NC}"
				continue
			fi
		fi
		if [ "${BUNDLE_UPDATED:-0}" = "1" ]; then
			echo -e "  ${YELLOW}⚠ bundle updated, forcing redistribution and reinstall${NC}"
		elif [ "${RESET_DB:-0}" = "1" ] || [ "${RESET_DB:-0}" = "true" ]; then
			echo -e "  ${YELLOW}⚠ RESET_DB=1, forcing reinstall of the compute node${NC}"
		fi

		local install_log
		install_log="$(mktemp "${TMPDIR:-/tmp}/cubesandbox_compute_${i}_install.XXXXXX.log")"
		local install_rc=0

		if [ -n "${LOCAL_BUNDLE:-}" ]; then
			# ---- Local bundle mode (compute node) ----
			echo -e "  ${CYAN}Using local bundle: ${LOCAL_BUNDLE}${NC}"

			# Upload vmlinux-pvm to the compute node
			if [ -f "${PVM_KERNEL_VMLINUX}" ]; then
				echo -e "  ${CYAN}Uploading vmlinux-pvm to the compute node /tmp/vmlinux-pvm...${NC}"
				scp -i "${key_file}" -P "${SSH_PORT}" "${JUMP_PROXY_OPTS[@]}" \
					-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
					-o ConnectTimeout=10 \
					"${PVM_KERNEL_VMLINUX}" "root@${compute_private_ip}:/tmp/vmlinux-pvm" 2>&1 || {
					echo -e "  ${RED}✗ vmlinux-pvm upload failed${NC}"
					failed_nodes+=("${compute_private_ip} (vmlinux upload)")
					continue
				}
				echo -e "  ${GREEN}✓ vmlinux-pvm upload complete${NC}"
			else
				echo -e "  ${YELLOW}⚠ ${PVM_KERNEL_VMLINUX} does not exist, skipping vmlinux upload${NC}"
			fi

			local bundle_name bundle_dir
			bundle_name="$(basename "${LOCAL_BUNDLE}")"
			bundle_dir="${bundle_name%.tar.gz}"

			echo -e "  ${CYAN}Uploading ${bundle_name} to the compute node...${NC}"
			scp -i "${key_file}" -P "${SSH_PORT}" "${JUMP_PROXY_OPTS[@]}" \
				-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
				-o ConnectTimeout=10 \
				"${LOCAL_BUNDLE}" "root@${compute_private_ip}:/tmp/${bundle_name}" 2>&1 || {
				echo -e "  ${RED}✗ SCP upload failed${NC}"
				failed_nodes+=("${compute_private_ip} (bundle upload)")
				continue
			}
			echo -e "  ${GREEN}✓ Upload complete${NC}"

			# Get the cube-master CLB VPC IP (preferred) or fall back to the control node's internal IP
			local control_plane_ip="$cm_clb_ip"
			echo -e "  ${CYAN}Control plane IP: ${control_plane_ip}${NC}"

			# Get MySQL/Redis info
			local mysql_ip redis_ip
			mysql_ip=$(terraform output -raw mysql_intranet_ip 2>/dev/null || echo "")
			redis_ip=$(terraform output -raw redis_intranet_ip 2>/dev/null || echo "")

			# Build the remote install command: extract → create .env → run install-compute.sh
			local remote_cmd="set -euo pipefail
echo '[local-bundle] Extracting...'
mkdir -p /tmp/cube-sandbox-bundle
tar -xzf '/tmp/${bundle_name}' -C /tmp/cube-sandbox-bundle/
BUNDLE_DIR=\$(ls -d /tmp/cube-sandbox-bundle/${bundle_dir} 2>/dev/null || ls -d /tmp/cube-sandbox-bundle/cube-sandbox-one-click-* 2>/dev/null | head -1)
echo \"[local-bundle] Bundle dir: \${BUNDLE_DIR}\""

			# When an override vmlinux-pvm was uploaded (bundle lacked one, or the
			# user provided an explicit kernel), inject it into the inner
			# sandbox-package.tar.gz so install.sh (select_installed_kernel_vmlinux)
			# picks it up from sandbox-package/cube-kernel-scf/vmlinux-pvm.
			remote_cmd+="
if [ -s /tmp/vmlinux-pvm ]; then
  PKG_TAR=\"\${BUNDLE_DIR}/assets/package/sandbox-package.tar.gz\"
  if [ -f \"\$PKG_TAR\" ]; then
    echo '[local-bundle] Injecting vmlinux-pvm into sandbox-package...'
    INJ=\$(mktemp -d)
    tar -xzf \"\$PKG_TAR\" -C \"\$INJ\"
    mkdir -p \"\$INJ/sandbox-package/cube-kernel-scf\"
    cp -f /tmp/vmlinux-pvm \"\$INJ/sandbox-package/cube-kernel-scf/vmlinux-pvm\"
    tar -C \"\$INJ\" -czf \"\$PKG_TAR\" sandbox-package
    rm -rf \"\$INJ\"
    echo '[local-bundle] vmlinux-pvm injected'
  fi
fi"

			remote_cmd+="
echo '[local-bundle] Creating .env...'
cd \"\${BUNDLE_DIR}\"
cat <<EOF > .env
CUBE_EXTERNAL_MYSQL_HOST=${mysql_ip}
CUBE_EXTERNAL_REDIS_HOST=${redis_ip}
CUBE_PVM_ENABLE=1
ONE_CLICK_CONTROL_PLANE_IP=\"${control_plane_ip}\"
MIRROR=${egress_mirror}
EOF
echo '[local-bundle] .env created:'
cat .env"

			remote_cmd+="
echo '[local-bundle] Running install-compute.sh...'
bash install-compute.sh 2>&1
echo '[local-bundle] Done'"

			# Disable errexit so the ssh/install exit code can be read from
			# PIPESTATUS (the remote script runs `set -euo pipefail`, so a failing
			# install-compute.sh makes ssh return non-zero) rather than guessing
			# success from log text. ssh is element [1] of echo|ssh|tee.
			set +e
			echo "$remote_cmd" | ssh "${ssh_opts[@]}" -o ConnectTimeout=15 root@"${compute_private_ip}" \
				"bash 2>&1" 2>&1 | tee "$install_log"
			install_rc=${PIPESTATUS[1]}
			set -e
		else
			# ---- Default online mode ----
			local cn_url="https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh"
			# `set -o pipefail` on the remote so a curl download failure (not just a
			# bash failure) propagates as a non-zero exit; ssh is element [0] of
			# ssh|tee. set +e locally so we can capture it from PIPESTATUS.
			set +e
			ssh "${ssh_opts[@]}" -o ConnectTimeout=15 root@"${compute_private_ip}" \
				"set -o pipefail; curl -fsSL --connect-timeout 10 --max-time 60 '${cn_url}' | \
         ONE_CLICK_DEPLOY_ROLE=compute \
         CUBE_SANDBOX_NODE_IP='${compute_private_ip}' \
         ONE_CLICK_CONTROL_PLANE_IP='${cm_clb_ip}' \
         CUBE_PVM_ENABLE=1 \
         MIRROR='${egress_mirror}' bash 2>&1" 2>&1 | tee "$install_log"
			install_rc=${PIPESTATUS[0]}
			set -e
		fi

		# The install output was already streamed live via `tee "$install_log"`;
		# the exit code (captured above) is the authoritative success signal.
		rm -f "$install_log"

		if [ "${install_rc:-1}" -eq 0 ]; then
			echo -e "  ${GREEN}✓ compute installation complete${NC}"
			echo -e "  ${CYAN}Configuring cubelet node status update frequency: ${CUBELET_NODE_STATUS_UPDATE_FREQUENCY:-1s}${NC}"
			if ssh "${ssh_opts[@]}" root@"${compute_private_ip}" "CUBELET_NODE_STATUS_UPDATE_FREQUENCY='${CUBELET_NODE_STATUS_UPDATE_FREQUENCY:-1s}' bash -s" <<'REMOTE_CUBELET_FREQ'
set -euo pipefail
freq="${CUBELET_NODE_STATUS_UPDATE_FREQUENCY:-1s}"
cfg="/usr/local/services/cubetoolbox/Cubelet/config/config.toml"
if [ ! -f "$cfg" ]; then
  echo "cubelet config not found: $cfg" >&2
  exit 1
fi
if command -v python3 >/dev/null 2>&1; then
  python3 - "$cfg" "$freq" <<'PY'
import re
import sys

path, freq = sys.argv[1], sys.argv[2]
with open(path, "r", encoding="utf-8") as f:
    text = f.read()

section_re = re.compile(r'(\[plugins\."io\.cubelet\.controller\.config\.v1\.cubelet"\]\n)(.*?)(?=\n\s*\[|$)', re.S)
match = section_re.search(text)
if not match:
    raise SystemExit("cubelet controller config section not found")

body = match.group(2)
line_re = re.compile(r'(^\s*node_status_update_frequency\s*=\s*)".*?"', re.M)
if line_re.search(body):
    body = line_re.sub(rf'\1"{freq}"', body, count=1)
else:
    body = body.rstrip() + f'\n    node_status_update_frequency = "{freq}"\n'

text = text[:match.start(2)] + body + text[match.end(2):]
with open(path, "w", encoding="utf-8") as f:
    f.write(text)
PY
else
  sed -i -E "s#^([[:space:]]*node_status_update_frequency[[:space:]]*=[[:space:]]*)\"[^\"]*\"#\1\"${freq}\"#" "$cfg"
fi
grep -q "node_status_update_frequency = \"${freq}\"" "$cfg"
systemctl restart cube-sandbox-cubelet.service
REMOTE_CUBELET_FREQ
			then
				echo -e "  ${GREEN}✓ cubelet node status update frequency configured${NC}"
			else
				echo -e "  ${RED}✗ failed to configure cubelet node status update frequency on node ${node_num}${NC}"
				failed_nodes+=("${compute_private_ip} (cubelet config)")
			fi
		else
			echo -e "  ${RED}✗ compute installation failed on node ${node_num} (${compute_private_ip}, exit ${install_rc})${NC}"
			failed_nodes+=("${compute_private_ip} (install)")
		fi

		# 4) Local verification
		echo ""
		echo -e "  ${CYAN}[${node_num}.4] Local verification...${NC}"
		echo -e "  ${CYAN}systemctl status:${NC}"
		ssh "${ssh_opts[@]}" root@"${compute_private_ip}" \
			"systemctl status cube-sandbox-compute.target --no-pager -l 2>&1 || true" 2>&1 || true

		# Tag
		if [ -n "$compute_id" ] && command -v tccli &>/dev/null; then
			tccli cvm ModifyInstancesAttribute \
				--InstanceIds "[\"${compute_id}\"]" \
				--Tags "[{\"Key\":\"CubeSandboxRole\",\"Value\":\"compute\"}]" \
				--region "${TENCENTCLOUD_REGION:-ap-guangzhou}" >/dev/null 2>&1 || true
			echo -e "  ${GREEN}✓ Tagged: CubeSandboxRole=compute${NC}"
		fi
		echo ""
	done

	# Health checks (node registration verification + template creation) rely on
	# cube-master, which is a TKE addon. If the image build/push failed the addons
	# were not deployed, so skip the health checks entirely.
	if [ "${IMAGES_OK:-1}" != "1" ]; then
		echo ""
		echo -e "  ${YELLOW}⚠ Image build/push failed earlier; skipping node registration verification and template creation${NC}"
		return 0
	fi

	# Verify that all compute nodes are registered with cube-master
	echo ""
	echo -e "  ${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
	echo -e "  ${CYAN}Node registration verification${NC}"
	echo -e "  ${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"

	if [ -z "${cm_clb_ip}" ]; then
		echo -e "  ${YELLOW}⚠ cube-master CLB unavailable, skipping verification${NC}"
	else
		local nodes_json
		# Query cube-master through the jumpserver
		nodes_json=$(_jump_exec "curl -s --connect-timeout 10 'http://${cm_clb_ip}:8089/internal/meta/nodes' 2>&1" 2>&1) || true

		# Output the registered nodes (with health status)
		local node_ips node_count node_status
		node_ips=$(echo "$nodes_json" | jq -r '.data[]?.node_id' 2>/dev/null || echo "")
		node_count=$(echo "$nodes_json" | jq -r '.data | length' 2>/dev/null || echo "0")
		node_status=$(echo "$nodes_json" | jq -r '.data[] | "  \(.node_id)  healthy=\(.healthy)"' 2>/dev/null || echo "")
		echo ""
		echo -e "  ${CYAN}Registered nodes (${node_count}):${NC}"
		echo "$node_status"
		echo ""
		echo -e "  ${CYAN}Expected compute nodes (${compute_count}): $(terraform output -json compute_private_ips 2>/dev/null | jq -r 'join(" ")' || echo "")${NC}"
		echo ""

		# Query the available resources of each node
		if [ -n "$node_ips" ]; then
			local mysql_host mysql_port mysql_user mysql_pass mysql_db
			mysql_host=$(terraform output -raw mysql_intranet_ip 2>/dev/null || echo "")
			mysql_port=$(terraform output -raw mysql_intranet_port 2>/dev/null || echo "3306")
			mysql_user="${CUBE_USER:-cube}"
			mysql_pass="${CUBE_PASSWORD:-cube_pass}"
			mysql_db="${CUBE_DB:-cube_mvp}"

			if [ -n "$mysql_host" ]; then
				# Only query nodes with healthy=true
				local healthy_node_ips node_list
				healthy_node_ips=$(echo "$nodes_json" | jq -r '.data[] | select(.healthy == true) | .node_id' 2>/dev/null || echo "")
				if [ -z "$healthy_node_ips" ]; then
					echo -e "  ${YELLOW}⚠ No healthy nodes${NC}"
				else
					node_list=$(echo "$healthy_node_ips" | grep -v '^$' | sed "s/^/'/" | sed "s/$/'/" | tr '\n' ',' | sed 's/,$//')
					echo -e "  ${CYAN}Node available resources (healthy only):${NC}"
					_jump_exec_stdin "${mysql_pass}" "MYSQL_PWD=\"\$(cat)\" mysql -h '${mysql_host}' -P '${mysql_port}' -u '${mysql_user}' '${mysql_db}' -e \"SELECT node_id, quota_cpu, quota_mem_mb FROM t_cube_node_registration WHERE node_id IN (${node_list});\" 2>&1"
				fi
				echo ""
			fi
		fi

		# Check the number of existing templates; create one if it is 0 (requires at least 1 registered node)
		if [ -n "${cm_clb_ip}" ]; then
			# Get the number of registered healthy nodes
			local healthy_count
			healthy_count=$(echo "$nodes_json" | jq -r '[.data[]? | select(.healthy == true)] | length' 2>/dev/null || echo "0")
			healthy_count=$(echo "$healthy_count" | tr -d ' \n\r')
			echo -e "  ${CYAN}Registered healthy nodes: ${healthy_count:-0}${NC}"

			if [ "${healthy_count:-0}" -lt 1 ] 2>/dev/null; then
				echo -e "  ${YELLOW}⚠ Not enough healthy nodes (<1), skipping template creation${NC}"
			else
				local tpl_count tpl_image="cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest"
				tpl_count=$(_jump_exec "cubemastercli --address ${cm_clb_ip} --port 8089 tpl ls 2>&1 | tail -n +2 | wc -l" 2>&1) || true
				tpl_count=$(echo "$tpl_count" | tr -d ' \n\r')
				echo -e "  ${CYAN}Existing templates: ${tpl_count:-0}${NC}"

				if [ "${tpl_count:-0}" -eq 0 ] 2>/dev/null; then
					echo -e "  ${CYAN}━━━ Create template ━━━${NC}"
					_jump_exec "cubemastercli --address ${cm_clb_ip} --port 8089 tpl create-from-image --image ${tpl_image} --with-cube-ca=false --writable-layer-size 1G --expose-port 49999 --expose-port 49983 --probe 49999 2>&1"
				else
					echo -e "  ${GREEN}✓ Template already exists, skipping creation${NC}"
					_jump_exec "cubemastercli --address ${cm_clb_ip} --port 8089 tpl ls 2>&1"
				fi
			fi
			echo ""
		fi
	fi

	# Node registration is the AUTHORITATIVE end-state signal. A per-node
	# installer exit code can be a false negative — e.g. the post-install
	# quickcheck probes a health endpoint that is momentarily not ready and
	# returns exit 1 — even though the node ultimately registered with
	# cube-master and reports healthy. So if EVERY expected compute node is
	# registered AND healthy (per the verification above), clear the install-stage
	# failures and continue instead of aborting an otherwise-working deployment.
	if [ "${#failed_nodes[@]}" -gt 0 ] && [ -n "${nodes_json:-}" ]; then
		local _expected_ip _healthy_ips _all_registered=1 _expected_n=0
		_healthy_ips=$(echo "$nodes_json" | jq -r '.data[]? | select(.healthy == true) | .node_id' 2>/dev/null || echo "")
		while IFS= read -r _expected_ip; do
			[ -n "$_expected_ip" ] || continue
			_expected_n=$((_expected_n + 1))
			echo "$_healthy_ips" | grep -qxF "$_expected_ip" || _all_registered=0
		done < <(printf '%s' "$compute_ips_json" | jq -r '.[]?' 2>/dev/null)

		if [ "$_all_registered" = "1" ] && [ "$_expected_n" -gt 0 ]; then
			echo ""
			echo -e "  ${GREEN}✓ All ${_expected_n} expected compute node(s) are registered and healthy with cube-master.${NC}"
			echo -e "  ${YELLOW}(The per-node installer reported a non-fatal error, but node registration confirms${NC}"
			echo -e "  ${YELLOW} the nodes are up — treating compute-node initialization as successful.)${NC}"
			failed_nodes=()
		fi
	fi

	# Fail fast if any compute node did not finish initialization, so the caller
	# does not print "deployment complete" for a cluster missing compute capacity.
	if [ "${#failed_nodes[@]}" -gt 0 ]; then
		echo -e "${RED}✗ ${#failed_nodes[@]} compute node(s) failed to initialize:${NC}"
		local _fn
		for _fn in "${failed_nodes[@]}"; do
			echo -e "    ${RED}- ${_fn}${NC}"
		done
		echo -e "  ${YELLOW}Fix the cause and re-run create.sh; already-initialized nodes are skipped.${NC}"
		return 1
	fi
	return 0
}

# ---------------------------------------------------------------
# Main
# ---------------------------------------------------------------
# ---------------------------------------------------------------
# ENV_FILE — records the user's selections so a deployment can be recreated with
#   the same configuration after a destroy.
# ---------------------------------------------------------------
ENV_FILE="${SCRIPT_DIR}/.env"
RESOLVED_TFVARS_FILE="${SCRIPT_DIR}/resolved.auto.tfvars.json"
RESOLVED_TFVARS_SUSPENDED=""

# ---------------------------------------------------------------
# load_saved_env — when re-running create.sh, preload previously saved selections
#   from ENV_FILE. Values already set in the current environment take precedence
#   (the file only fills in what is unset), so an explicit override still wins.
# ---------------------------------------------------------------
load_saved_env() {
	[ -f "${ENV_FILE}" ] || return 0
	echo -e "${CYAN}Found previous selections in ${ENV_FILE}; reusing them (only for unset values).${NC}"
	# Parse loop lives in lib-state-sync.sh so create.sh and destroy.sh stay in sync.
	_load_env_file "${ENV_FILE}"
}

# ---------------------------------------------------------------
# save_env_file — persist the resolved user selections to ENV_FILE so the same
#   configuration can be recreated after a destroy. Values are single-quoted to
#   survive spaces (e.g. the OS image regex). The file may contain secrets, so it
#   is written with 0600 permissions.
# ---------------------------------------------------------------
save_env_file() {
	# Pass "quiet" to suppress the confirmation message (used for the early,
	# pre-provisioning save so the output is not duplicated).
	local quiet="${1:-}"

	local az js_az cmp_az tke_az cfg
	# Read the config_summary output ONCE (each `terraform output` re-parses the
	# whole state), then pull the four zones out of the cached JSON.
	cfg=$(terraform output -json config_summary 2>/dev/null || echo "")
	az=$(echo "$cfg" | jq -r '.availability_zone // empty' 2>/dev/null || echo "")
	js_az=$(echo "$cfg" | jq -r '.jumpserver_availability_zone // empty' 2>/dev/null || echo "")
	cmp_az=$(echo "$cfg" | jq -r '.compute_availability_zone // empty' 2>/dev/null || echo "")
	tke_az=$(echo "$cfg" | jq -r '.tke_worker_availability_zone // empty' 2>/dev/null || echo "")
	[ -z "$az" ] && az="${TF_VAR_availability_zone:-${TENCENTCLOUD_AVAILABILITY_ZONE:-}}"
	[ -z "$js_az" ] && js_az="${TF_VAR_jumpserver_availability_zone:-${TENCENTCLOUD_JUMPSERVER_AVAILABILITY_ZONE:-$az}}"
	[ -z "$cmp_az" ] && cmp_az="${TF_VAR_compute_availability_zone:-${TENCENTCLOUD_COMPUTE_AVAILABILITY_ZONE:-$az}}"
	[ -z "$tke_az" ] && tke_az="${TF_VAR_tke_worker_availability_zone:-${TENCENTCLOUD_TKE_WORKER_AVAILABILITY_ZONE:-$az}}"

	(
		umask 077
		cat >"${ENV_FILE}" <<EOF
# CubeSandbox deployment selections, generated by create.sh on $(date '+%Y-%m-%d %H:%M:%S')
# Re-create with the same configuration: ./create.sh (this file is auto-loaded)
# WARNING: contains credentials and passwords; keep it private, do not commit.
TENCENTCLOUD_SECRET_ID='${TENCENTCLOUD_SECRET_ID:-}'
TENCENTCLOUD_SECRET_KEY='${TENCENTCLOUD_SECRET_KEY:-}'
TENCENTCLOUD_REGION='${TF_VAR_region:-${TENCENTCLOUD_REGION:-ap-guangzhou}}'
TENCENTCLOUD_VPC_NAME='${TF_VAR_vpc_name:-${TENCENTCLOUD_VPC_NAME:-cubesandbox-terraform-vpc}}'
TENCENTCLOUD_AVAILABILITY_ZONE='${az}'
TENCENTCLOUD_JUMPSERVER_AVAILABILITY_ZONE='${js_az}'
TENCENTCLOUD_COMPUTE_AVAILABILITY_ZONE='${cmp_az}'
TENCENTCLOUD_TKE_WORKER_AVAILABILITY_ZONE='${tke_az}'
TENCENTCLOUD_IMAGE_NAME='${TENCENTCLOUD_IMAGE_NAME:-OpenCloudOS Server 9}'
TENCENTCLOUD_JUMPSERVER_INSTANCE_TYPE='${TF_VAR_jumpserver_instance_type:-${TENCENTCLOUD_JUMPSERVER_INSTANCE_TYPE:-}}'
TENCENTCLOUD_COMPUTE_INSTANCE_TYPE='${TF_VAR_compute_instance_type:-${TENCENTCLOUD_COMPUTE_INSTANCE_TYPE:-}}'
TENCENTCLOUD_COMPUTE_NODE_COUNT='${saved_compute_count:-${TF_VAR_compute_node_count:-2}}'
TENCENTCLOUD_MYSQL_PASSWORD='${TENCENTCLOUD_MYSQL_PASSWORD:-}'
TENCENTCLOUD_REDIS_PASSWORD='${TENCENTCLOUD_REDIS_PASSWORD:-}'
TENCENTCLOUD_CUBE_DB='${TENCENTCLOUD_CUBE_DB:-cube_mvp}'
TENCENTCLOUD_CUBE_USER='${TENCENTCLOUD_CUBE_USER:-cube}'
TENCENTCLOUD_CUBE_PASSWORD='${TENCENTCLOUD_CUBE_PASSWORD:-}'
TENCENTCLOUD_CUBELET_NODE_STATUS_UPDATE_FREQUENCY='${CUBELET_NODE_STATUS_UPDATE_FREQUENCY:-${TENCENTCLOUD_CUBELET_NODE_STATUS_UPDATE_FREQUENCY:-1s}}'
TENCENTCLOUD_CUBE_IMAGE_TAG='${TENCENTCLOUD_CUBE_IMAGE_TAG:-v0.5.1}'
TENCENTCLOUD_IMAGE_REGISTRY='${TF_VAR_image_registry:-${TENCENTCLOUD_IMAGE_REGISTRY:-cube-sandbox-cn.tencentcloudcr.com}}'
TENCENTCLOUD_IMAGE_NAMESPACE='${TF_VAR_image_namespace:-${TENCENTCLOUD_IMAGE_NAMESPACE:-cube-sandbox}}'
TENCENTCLOUD_CUBEMASTER_IMAGE='${TF_VAR_cubemaster_image:-${TENCENTCLOUD_CUBEMASTER_IMAGE:-}}'
TENCENTCLOUD_CUBEAPI_IMAGE='${TF_VAR_cubeapi_image:-${TENCENTCLOUD_CUBEAPI_IMAGE:-}}'
TENCENTCLOUD_CUBEPROXY_IMAGE='${TF_VAR_cubeproxy_image:-${TENCENTCLOUD_CUBEPROXY_IMAGE:-}}'
TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_IMAGE='${TF_VAR_cube_lifecycle_manager_image:-${TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_IMAGE:-}}'
TENCENTCLOUD_WEBUI_IMAGE='${TF_VAR_webui_image:-${TENCENTCLOUD_WEBUI_IMAGE:-}}'
TENCENTCLOUD_TKE_CLUSTER_VERSION='${TKE_CLUSTER_VERSION:-1.34.1}'
TENCENTCLOUD_TKE_NODE_COUNT='${TKE_NODE_COUNT:-2}'
TENCENTCLOUD_CUBEMASTER_REPLICAS='${TENCENTCLOUD_CUBEMASTER_REPLICAS:-1}'
TENCENTCLOUD_CUBE_API_REPLICAS='${TF_VAR_cube_api_replicas:-${TENCENTCLOUD_CUBE_API_REPLICAS:-1}}'
TENCENTCLOUD_CUBE_PROXY_REPLICAS='${TF_VAR_cube_proxy_replicas:-${TENCENTCLOUD_CUBE_PROXY_REPLICAS:-1}}'
TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_REPLICAS='${TF_VAR_cube_lifecycle_manager_replicas:-${TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_REPLICAS:-1}}'
TENCENTCLOUD_CUBE_WEBUI_REPLICAS='${TF_VAR_cube_webui_replicas:-${TENCENTCLOUD_CUBE_WEBUI_REPLICAS:-1}}'
TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_DEFAULT_IDLE_TIMEOUT='${TF_VAR_cube_lifecycle_manager_default_idle_timeout:-${TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_DEFAULT_IDLE_TIMEOUT:-5m}}'
TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_HEARTBEAT_TTL='${TF_VAR_cube_lifecycle_manager_heartbeat_ttl:-${TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_HEARTBEAT_TTL:-15s}}'
TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_DISCOVERY_REFRESH='${TF_VAR_cube_lifecycle_manager_discovery_refresh:-${TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_DISCOVERY_REFRESH:-3s}}'
TENCENTCLOUD_CUBE_ADMIN_TOKEN='${TF_VAR_cube_admin_token:-${TENCENTCLOUD_CUBE_ADMIN_TOKEN:-}}'
TENCENTCLOUD_CUBE_PROXY_HEARTBEAT_INTERVAL_MS='${TF_VAR_cube_proxy_heartbeat_interval_ms:-${TENCENTCLOUD_CUBE_PROXY_HEARTBEAT_INTERVAL_MS:-5000}}'
TENCENTCLOUD_ENABLE_PUBLIC_NETWORK='${TF_VAR_enable_public_network:-${TENCENTCLOUD_ENABLE_PUBLIC_NETWORK:-false}}'
TENCENTCLOUD_LOCAL_BUNDLE='${LOCAL_BUNDLE:-${TENCENTCLOUD_LOCAL_BUNDLE:-}}'
TENCENTCLOUD_PVM_KERNEL_VMLINUX='${PVM_KERNEL_VMLINUX:-${TENCENTCLOUD_PVM_KERNEL_VMLINUX:-}}'
TENCENTCLOUD_VERBOSE='${VERBOSE:-1}'
EOF
	)
	chmod 600 "${ENV_FILE}" 2>/dev/null || true

	# Every value is written single-quoted and UNESCAPED, and reloaded by
	# _env_value (lib-state-sync.sh), which reads only the text INSIDE the quotes.
	# A value that itself contains a single quote (e.g. a password) would therefore
	# reload truncated on the next create/destroy and silently drift the apply.
	# A well-formed data line has exactly two single quotes; flag anything else now
	# (this runs at the early pre-provisioning save) with a fixable message, naming
	# the key but never echoing the value.
	local _line _quotes
	while IFS= read -r _line; do
		case "$_line" in
		"" | \#*) continue ;;
		esac
		_quotes="${_line//[!\']/}"
		if [ "${#_quotes}" -ne 2 ]; then
			echo -e "${RED}✗ ${_line%%=*} contains a single quote ('), which is not supported.${NC}" >&2
			echo -e "  ${YELLOW}Values are stored single-quoted in ${ENV_FILE} and cannot round-trip an embedded single quote.${NC}" >&2
			echo -e "  ${YELLOW}Please choose a value without a single quote and re-run.${NC}" >&2
			exit 1
		fi
	done <"${ENV_FILE}"

	if [ "$quiet" != "quiet" ]; then
		echo ""
		echo -e "${GREEN}✓ Saved your selections to ${ENV_FILE}${NC}"
		echo -e "  ${CYAN}After destroy.sh, you may re-run ./create.sh to recreate with the same config.${NC}"
	fi
}

# ---------------------------------------------------------------
# write_resolved_tfvars_file — persist the ACTUAL resolved Terraform variables.
#
# .env records the user's preferred inputs, but auto-fallback can purchase a
# different compute/TKE instance type or zone. The generated
# resolved.auto.tfvars.json captures the real deployed shape so later raw
# terraform plan/apply runs do not drift back to stale .env preferences. It is
# mode 0600 because it can contain DB/Redis passwords.
# ---------------------------------------------------------------
_tf_var_declared() {
	grep -qE "^variable[[:space:]]+\"$1\"" "${SCRIPT_DIR}/variables.tf" 2>/dev/null
}

_json_or_default() {
	local value="$1" default="$2"
	if [ -n "$value" ] && printf '%s' "$value" | jq -e . >/dev/null 2>&1; then
		printf '%s' "$value"
	else
		printf '%s' "$default"
	fi
}

_number_or_default() {
	local value="$1" default="$2"
	if [[ "$value" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
		printf '%s' "$value"
	else
		printf '%s' "$default"
	fi
}

_bool_json() {
	case "${1:-}" in
	true | TRUE | True | 1 | yes | YES | y | Y) printf 'true' ;;
	*) printf 'false' ;;
	esac
}

_jq_add_string_if_declared() {
	local file="$1" key="$2" value="$3" tmp
	_tf_var_declared "$key" || return 0
	[ -n "$value" ] || return 0
	tmp="$(mktemp "${TMPDIR:-/tmp}/resolved_tfvars.XXXXXX")"
	jq --arg k "$key" --arg v "$value" '. + {($k): $v}' "$file" >"$tmp" && mv "$tmp" "$file"
}

_jq_add_number_if_declared() {
	local file="$1" key="$2" value="$3" tmp
	_tf_var_declared "$key" || return 0
	[[ "$value" =~ ^[0-9]+([.][0-9]+)?$ ]] || return 0
	tmp="$(mktemp "${TMPDIR:-/tmp}/resolved_tfvars.XXXXXX")"
	jq --arg k "$key" --argjson v "$value" '. + {($k): $v}' "$file" >"$tmp" && mv "$tmp" "$file"
}

_jq_add_bool_if_declared() {
	local file="$1" key="$2" value="$3" tmp bool
	_tf_var_declared "$key" || return 0
	[ -n "$value" ] || return 0
	bool="$(_bool_json "$value")"
	tmp="$(mktemp "${TMPDIR:-/tmp}/resolved_tfvars.XXXXXX")"
	jq --arg k "$key" --argjson v "$bool" '. + {($k): $v}' "$file" >"$tmp" && mv "$tmp" "$file"
}

restore_suspended_resolved_tfvars() {
	if [ -n "${RESOLVED_TFVARS_SUSPENDED:-}" ] && [ ! -f "$RESOLVED_TFVARS_FILE" ] && [ -f "$RESOLVED_TFVARS_SUSPENDED" ]; then
		mv "$RESOLVED_TFVARS_SUSPENDED" "$RESOLVED_TFVARS_FILE" 2>/dev/null || true
	fi
}

load_resolved_tfvars_file() {
	[ -f "$RESOLVED_TFVARS_FILE" ] || return 0
	echo -e "${CYAN}Found ${RESOLVED_TFVARS_FILE}; using it as the resolved deployment baseline.${NC}"

	local entry key value var
	while IFS= read -r entry; do
		key="$(printf '%s' "$entry" | base64 -d | jq -r '.key' 2>/dev/null || true)"
		[ -n "$key" ] || continue
		case "$key" in
		create_tke | deploy_tke_addons) continue ;;
		esac
		_tf_var_declared "$key" || continue
		value="$(printf '%s' "$entry" | base64 -d | jq -r 'if (.value | type) == "string" then .value else (.value | tojson) end' 2>/dev/null || true)"
		var="TF_VAR_${key}"
		export "${var}=${value}"
	done < <(jq -r 'to_entries[] | @base64' "$RESOLVED_TFVARS_FILE" 2>/dev/null || true)

	# Terraform auto-loads *.auto.tfvars.json with higher precedence than TF_VAR_*.
	# During create.sh we need fallback code to keep changing TF_VAR_* between
	# phased applies, so suspend auto-loading and restore the file on failure. A
	# successful run writes a fresh resolved.auto.tfvars.json at the end.
	RESOLVED_TFVARS_SUSPENDED="${RESOLVED_TFVARS_FILE}.suspended"
	mv "$RESOLVED_TFVARS_FILE" "$RESOLVED_TFVARS_SUSPENDED"
	trap 'restore_suspended_resolved_tfvars' EXIT
	echo -e "  ${CYAN}Suspended auto-loading while create.sh runs; it will be regenerated on success.${NC}"
}

write_resolved_tfvars_file() {
	local cfg compute_types_json compute_zones_json tmp
	local az js_az cmp_az tke_az jump_type compute_pref
	local compute_count tke_count

	cfg="$(terraform output -json config_summary 2>/dev/null || echo '{}')"
	compute_types_json="$(terraform output -json compute_instance_types 2>/dev/null || printf '%s' "${TF_VAR_compute_instance_types:-[]}")"
	compute_zones_json="$(terraform output -json compute_availability_zones 2>/dev/null || printf '%s' "${TF_VAR_compute_availability_zones:-[]}")"
	compute_types_json="$(_json_or_default "$compute_types_json" '[]')"
	compute_zones_json="$(_json_or_default "$compute_zones_json" '[]')"

	az="$(printf '%s' "$cfg" | jq -r '.availability_zone // empty' 2>/dev/null || true)"
	js_az="$(printf '%s' "$cfg" | jq -r '.jumpserver_availability_zone // empty' 2>/dev/null || true)"
	cmp_az="$(printf '%s' "$cfg" | jq -r '.compute_availability_zone // empty' 2>/dev/null || true)"
	tke_az="$(printf '%s' "$cfg" | jq -r '.tke_worker_availability_zone // empty' 2>/dev/null || true)"
	jump_type="$(printf '%s' "$cfg" | jq -r '.jumpserver_instance_type // empty' 2>/dev/null || true)"
	compute_pref="$(printf '%s' "$cfg" | jq -r '.compute_instance_type // empty' 2>/dev/null || true)"

	[ -n "$az" ] || az="${TF_VAR_availability_zone:-${TENCENTCLOUD_AVAILABILITY_ZONE:-}}"
	[ -n "$js_az" ] || js_az="${TF_VAR_jumpserver_availability_zone:-${TENCENTCLOUD_JUMPSERVER_AVAILABILITY_ZONE:-$az}}"
	[ -n "$cmp_az" ] || cmp_az="${TF_VAR_compute_availability_zone:-${TENCENTCLOUD_COMPUTE_AVAILABILITY_ZONE:-$az}}"
	[ -n "$tke_az" ] || tke_az="${TF_VAR_tke_worker_availability_zone:-${TENCENTCLOUD_TKE_WORKER_AVAILABILITY_ZONE:-$az}}"
	[ -n "$jump_type" ] || jump_type="${TF_VAR_jumpserver_instance_type:-${TENCENTCLOUD_JUMPSERVER_INSTANCE_TYPE:-}}"
	[ -n "$compute_pref" ] || compute_pref="${TF_VAR_compute_instance_type:-${TENCENTCLOUD_COMPUTE_INSTANCE_TYPE:-}}"

	compute_count="${saved_compute_count:-${TF_VAR_compute_node_count:-${TENCENTCLOUD_COMPUTE_NODE_COUNT:-2}}}"
	tke_count="${TF_VAR_tke_node_count:-${TENCENTCLOUD_TKE_NODE_COUNT:-${TKE_NODE_COUNT:-2}}}"
	compute_count="$(_number_or_default "$compute_count" 1)"
	tke_count="$(_number_or_default "$tke_count" 0)"

	tmp="$(mktemp "${TMPDIR:-/tmp}/resolved_tfvars.XXXXXX")"
	jq -n \
		--arg vpc_name "${TF_VAR_vpc_name:-${TENCENTCLOUD_VPC_NAME:-cubesandbox-terraform-vpc}}" \
		--arg region "${TF_VAR_region:-${TENCENTCLOUD_REGION:-ap-guangzhou}}" \
		--arg availability_zone "$az" \
		--arg jumpserver_availability_zone "$js_az" \
		--arg compute_availability_zone "$cmp_az" \
		--arg tke_worker_availability_zone "$tke_az" \
		--arg image_name_regex "${TF_VAR_image_name_regex:-${TENCENTCLOUD_IMAGE_NAME:-OpenCloudOS Server 9}}" \
		--arg jumpserver_instance_type "$jump_type" \
		--arg compute_instance_type "$compute_pref" \
		--argjson compute_instance_types "$compute_types_json" \
		--argjson compute_availability_zones "$compute_zones_json" \
		--arg ssh_public_key_path "${TF_VAR_ssh_public_key_path:-$SSH_PUB_KEY}" \
		--arg ssh_private_key_path "${TF_VAR_ssh_private_key_path:-$SSH_PRI_KEY}" \
		--argjson compute_node_count "$compute_count" \
		--argjson compute_data_disk_size "$(_number_or_default "${TF_VAR_compute_data_disk_size:-${TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE:-200}}" 200)" \
		--arg mysql_root_password "${TF_VAR_mysql_root_password:-${TENCENTCLOUD_MYSQL_PASSWORD:-CubeSandbox123!}}" \
		--arg redis_password "${TF_VAR_redis_password:-${TENCENTCLOUD_REDIS_PASSWORD:-ceuhvu123}}" \
		--arg cube_password "${TF_VAR_cube_password:-${CUBE_PASSWORD:-${TENCENTCLOUD_CUBE_PASSWORD:-cube_pass}}}" \
		--arg cube_db "${TF_VAR_cube_db:-${CUBE_DB:-${TENCENTCLOUD_CUBE_DB:-cube_mvp}}}" \
		--arg cube_user "${TF_VAR_cube_user:-${CUBE_USER:-${TENCENTCLOUD_CUBE_USER:-cube}}}" \
		--arg cubelet_node_status_update_frequency "${TF_VAR_cubelet_node_status_update_frequency:-${CUBELET_NODE_STATUS_UPDATE_FREQUENCY:-${TENCENTCLOUD_CUBELET_NODE_STATUS_UPDATE_FREQUENCY:-1s}}}" \
		--arg tke_cluster_name "${TF_VAR_tke_cluster_name:-cubesandbox-terraform-tke}" \
		--arg tke_cluster_version "${TF_VAR_tke_cluster_version:-${TKE_CLUSTER_VERSION:-${TENCENTCLOUD_TKE_CLUSTER_VERSION:-1.34.1}}}" \
		--argjson tke_node_count "$tke_count" \
		--arg tke_worker_instance_type "${TF_VAR_tke_worker_instance_type:-${TENCENTCLOUD_TKE_WORKER_INSTANCE_TYPE:-SA9.LARGE8}}" \
		--arg tke_cluster_cidr "${TF_VAR_tke_cluster_cidr:-10.200.0.0/16}" \
		--arg tke_service_cidr "${TF_VAR_tke_service_cidr:-192.168.0.0/20}" \
		--argjson enable_public_network "$(_bool_json "${TF_VAR_enable_public_network:-${TENCENTCLOUD_ENABLE_PUBLIC_NETWORK:-false}}")" \
		--argjson use_tcr "$(_bool_json "${TF_VAR_use_tcr:-${TENCENTCLOUD_USE_TCR:-false}}")" \
		--argjson use_cfs "$(_bool_json "${TF_VAR_use_cfs:-${TENCENTCLOUD_USE_CFS:-false}}")" \
		--arg image_tag "${TF_VAR_image_tag:-${CUBE_IMAGE_TAG:-${TENCENTCLOUD_CUBE_IMAGE_TAG:-v0.5.1}}}" \
		--arg image_registry "${TF_VAR_image_registry:-${TENCENTCLOUD_IMAGE_REGISTRY:-cube-sandbox-cn.tencentcloudcr.com}}" \
		--arg image_namespace "${TF_VAR_image_namespace:-${TENCENTCLOUD_IMAGE_NAMESPACE:-cube-sandbox}}" \
		--arg cubemaster_image "${TF_VAR_cubemaster_image:-${TENCENTCLOUD_CUBEMASTER_IMAGE:-}}" \
		--arg cubeapi_image "${TF_VAR_cubeapi_image:-${TENCENTCLOUD_CUBEAPI_IMAGE:-}}" \
		--arg cubeproxy_image "${TF_VAR_cubeproxy_image:-${TENCENTCLOUD_CUBEPROXY_IMAGE:-}}" \
		--arg cube_lifecycle_manager_image "${TF_VAR_cube_lifecycle_manager_image:-${TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_IMAGE:-}}" \
		--arg webui_image "${TF_VAR_webui_image:-${TENCENTCLOUD_WEBUI_IMAGE:-}}" \
		--argjson cubemaster_replicas "$(_number_or_default "${TF_VAR_cubemaster_replicas:-${TENCENTCLOUD_CUBEMASTER_REPLICAS:-1}}" 1)" \
		--argjson cube_api_replicas "$(_number_or_default "${TF_VAR_cube_api_replicas:-${TENCENTCLOUD_CUBE_API_REPLICAS:-1}}" 1)" \
		--argjson cube_proxy_replicas "$(_number_or_default "${TF_VAR_cube_proxy_replicas:-${TENCENTCLOUD_CUBE_PROXY_REPLICAS:-1}}" 1)" \
		--argjson cube_lifecycle_manager_replicas "$(_number_or_default "${TF_VAR_cube_lifecycle_manager_replicas:-${TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_REPLICAS:-1}}" 1)" \
		--argjson cube_webui_replicas "$(_number_or_default "${TF_VAR_cube_webui_replicas:-${TENCENTCLOUD_CUBE_WEBUI_REPLICAS:-1}}" 1)" \
		--arg cube_lifecycle_manager_default_idle_timeout "${TF_VAR_cube_lifecycle_manager_default_idle_timeout:-${TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_DEFAULT_IDLE_TIMEOUT:-5m}}" \
		--arg cube_lifecycle_manager_heartbeat_ttl "${TF_VAR_cube_lifecycle_manager_heartbeat_ttl:-${TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_HEARTBEAT_TTL:-15s}}" \
		--arg cube_lifecycle_manager_discovery_refresh "${TF_VAR_cube_lifecycle_manager_discovery_refresh:-${TENCENTCLOUD_CUBE_LIFECYCLE_MANAGER_DISCOVERY_REFRESH:-3s}}" \
		--arg cube_admin_token "${TF_VAR_cube_admin_token:-${TENCENTCLOUD_CUBE_ADMIN_TOKEN:-}}" \
		--argjson cube_proxy_heartbeat_interval_ms "$(_number_or_default "${TF_VAR_cube_proxy_heartbeat_interval_ms:-${TENCENTCLOUD_CUBE_PROXY_HEARTBEAT_INTERVAL_MS:-5000}}" 5000)" \
		'{
			vpc_name: $vpc_name,
			region: $region,
			availability_zone: $availability_zone,
			jumpserver_availability_zone: $jumpserver_availability_zone,
			compute_availability_zone: $compute_availability_zone,
			tke_worker_availability_zone: $tke_worker_availability_zone,
			image_name_regex: $image_name_regex,
			jumpserver_instance_type: $jumpserver_instance_type,
			compute_instance_type: $compute_instance_type,
			compute_instance_types: $compute_instance_types,
			compute_availability_zones: $compute_availability_zones,
			ssh_public_key_path: $ssh_public_key_path,
			ssh_private_key_path: $ssh_private_key_path,
			compute_node_count: $compute_node_count,
			compute_data_disk_size: $compute_data_disk_size,
			mysql_root_password: $mysql_root_password,
			redis_password: $redis_password,
			cube_password: $cube_password,
			cube_db: $cube_db,
			cube_user: $cube_user,
			cubelet_node_status_update_frequency: $cubelet_node_status_update_frequency,
			tke_cluster_name: $tke_cluster_name,
			tke_cluster_version: $tke_cluster_version,
			tke_node_count: $tke_node_count,
			tke_worker_instance_type: $tke_worker_instance_type,
			tke_cluster_cidr: $tke_cluster_cidr,
			tke_service_cidr: $tke_service_cidr,
			enable_public_network: $enable_public_network,
			use_tcr: $use_tcr,
			use_cfs: $use_cfs,
			image_tag: $image_tag,
			image_registry: $image_registry,
			image_namespace: $image_namespace,
			cubemaster_image: $cubemaster_image,
			cubeapi_image: $cubeapi_image,
			cubeproxy_image: $cubeproxy_image,
			cube_lifecycle_manager_image: $cube_lifecycle_manager_image,
			webui_image: $webui_image,
			cubemaster_replicas: $cubemaster_replicas,
			cube_api_replicas: $cube_api_replicas,
			cube_proxy_replicas: $cube_proxy_replicas,
			cube_lifecycle_manager_replicas: $cube_lifecycle_manager_replicas,
			cube_webui_replicas: $cube_webui_replicas,
			cube_lifecycle_manager_default_idle_timeout: $cube_lifecycle_manager_default_idle_timeout,
			cube_lifecycle_manager_heartbeat_ttl: $cube_lifecycle_manager_heartbeat_ttl,
			cube_lifecycle_manager_discovery_refresh: $cube_lifecycle_manager_discovery_refresh,
			cube_admin_token: $cube_admin_token,
			cube_proxy_heartbeat_interval_ms: $cube_proxy_heartbeat_interval_ms,
		}
		| with_entries(select(.value != "" and .value != null))' >"$tmp"

	(
		umask 077
		[ -n "${RESOLVED_TFVARS_SUSPENDED:-}" ] && rm -f "$RESOLVED_TFVARS_SUSPENDED" 2>/dev/null || true
		mv "$tmp" "$RESOLVED_TFVARS_FILE"
	)
	chmod 600 "$RESOLVED_TFVARS_FILE" 2>/dev/null || true
	echo -e "  ${GREEN}✓ Resolved Terraform variables saved to ${RESOLVED_TFVARS_FILE}${NC}"
	echo -e "  ${CYAN}  This captures fallback-selected types/zones/counts/images for future Terraform runs.${NC}"
}

# ---------------------------------------------------------------
# Intranet-only kube-apiserver access.
#   The TKE cluster exposes its kube-apiserver on the INTRANET only
#   (cluster_internet=false, cluster_intranet=true in main.tf). This script runs
#   OUTSIDE the VPC, so it cannot reach that endpoint directly. The helpers below
#   open an SSH local-forward tunnel THROUGH the jumpserver (which sits inside the
#   VPC) and rewrite the LOCAL kubeconfig to talk to the tunnel, so the terraform
#   `kubernetes` provider and the API-server probe keep working from here.
#   The kubeconfig uploaded to the jumpserver itself is left untouched — the
#   jumpserver reaches the intranet endpoint directly.
# ---------------------------------------------------------------
APISERVER_LOCAL_PORT="${APISERVER_LOCAL_PORT:-6443}"
APISERVER_TUNNEL_PID=""
APISERVER_PROBE_URL=""
APISERVER_REMOTE_HOSTPORT="" # <intranet-apiserver-host>:<port>, remembered for restarts

# _close_apiserver_tunnel — tear down the SSH tunnel opened by _start_apiserver_tunnel.
_close_apiserver_tunnel() {
	[ -n "${APISERVER_TUNNEL_PID}" ] || return 0
	kill "${APISERVER_TUNNEL_PID}" 2>/dev/null || true
	APISERVER_TUNNEL_PID=""
}

# _localize_kubeconfig — point the LOCAL kubeconfig at the tunnel and skip TLS
#   verification (traffic is already protected by the SSH tunnel and stays inside
#   the VPC, which also avoids cert-SAN/DNS issues). Idempotent: a no-op once the
#   server already points at 127.0.0.1:${APISERVER_LOCAL_PORT}. The CA is dropped
#   because it is mutually exclusive with insecure-skip-tls-verify.
_localize_kubeconfig() {
	local kubeconfig="${SCRIPT_DIR}/.kube/config" cur
	[ -f "$kubeconfig" ] || return 0
	cur=$(grep -E '^[[:space:]]*server:[[:space:]]*' "$kubeconfig" | head -n1)
	case "$cur" in
	*"127.0.0.1:${APISERVER_LOCAL_PORT}"*) return 0 ;; # already localized
	esac
	local tmp
	tmp="${kubeconfig}.tmp.$$"
	awk -v port="${APISERVER_LOCAL_PORT}" '
		/^[ \t]*certificate-authority-data:/ { next }
		/^[ \t]*insecure-skip-tls-verify:/ { next }
		/^[ \t]*server:[ \t]*https?:\/\// {
			indent = $0
			sub(/[^ \t].*/, "", indent)
			print indent "server: https://127.0.0.1:" port
			print indent "insecure-skip-tls-verify: true"
			next
		}
		{ print }
	' "$kubeconfig" >"$tmp" && mv "$tmp" "$kubeconfig"
	chmod 600 "$kubeconfig" 2>/dev/null || true
}

# _start_apiserver_tunnel — (re)start the SSH local-forward
#   (127.0.0.1:${APISERVER_LOCAL_PORT} → ${APISERVER_REMOTE_HOSTPORT}) through the
#   jumpserver, then localize the kubeconfig. Used both for the initial open and
#   to re-establish a dropped tunnel.
_start_apiserver_tunnel() {
	local js_ip key_file host port
	[ -n "${APISERVER_REMOTE_HOSTPORT}" ] || return 1
	host="${APISERVER_REMOTE_HOSTPORT%:*}"
	port="${APISERVER_REMOTE_HOSTPORT##*:}"
	js_ip=$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "")
	key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"
	[ -n "$js_ip" ] || return 1
	_close_apiserver_tunnel
	ssh -i "$key_file" -p 443 \
		-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o ConnectTimeout=10 -o ExitOnForwardFailure=yes -o BatchMode=yes \
		-o ServerAliveInterval=30 -o ServerAliveCountMax=4 \
		-fN -L "127.0.0.1:${APISERVER_LOCAL_PORT}:${host}:${port}" \
		root@"${js_ip}" 2>/dev/null || return 1
	APISERVER_TUNNEL_PID=$(pgrep -f "127.0.0.1:${APISERVER_LOCAL_PORT}:${host}:${port}" 2>/dev/null | head -n1 || echo "")
	APISERVER_PROBE_URL="https://127.0.0.1:${APISERVER_LOCAL_PORT}"
	trap '_close_apiserver_tunnel; restore_suspended_resolved_tfvars' EXIT
	_localize_kubeconfig
	return 0
}

# _open_apiserver_tunnel — derive the intranet apiserver host:port from the LOCAL
#   kubeconfig's `server:` line (the intranet pgw endpoint), then open the tunnel
#   and localize the kubeconfig. Sets APISERVER_PROBE_URL.
_open_apiserver_tunnel() {
	local kubeconfig="${SCRIPT_DIR}/.kube/config"
	if [ -z "$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "")" ]; then
		echo -e "  ${YELLOW}⚠ jumpserver IP unknown; cannot tunnel to the intranet API Server${NC}"
		return 1
	fi
	if [ ! -f "$kubeconfig" ]; then
		echo -e "  ${YELLOW}⚠ local kubeconfig not found; cannot set up the API Server tunnel${NC}"
		return 1
	fi

	local server host port
	server=$(grep -E '^[[:space:]]*server:[[:space:]]*https?://' "$kubeconfig" |
		head -n1 | sed -E 's#^[[:space:]]*server:[[:space:]]*https?://##' | tr -d '[:space:]')
	case "$server" in
	127.0.0.1:*)
		# Already localized this run — just make sure the tunnel is alive.
		if [ -n "${APISERVER_REMOTE_HOSTPORT}" ]; then
			if [ -n "${APISERVER_TUNNEL_PID}" ] && kill -0 "${APISERVER_TUNNEL_PID}" 2>/dev/null; then
				APISERVER_PROBE_URL="https://127.0.0.1:${APISERVER_LOCAL_PORT}"
				return 0
			fi
			_start_apiserver_tunnel
			return $?
		fi
		echo -e "  ${YELLOW}⚠ kubeconfig already points at a local tunnel but the endpoint is unknown${NC}"
		return 1
		;;
	esac
	server="${server%%/*}" # strip any path
	host="${server%:*}"
	port="${server##*:}"
	[ "$port" = "$server" ] && port="443"
	if [ -z "$host" ]; then
		echo -e "  ${YELLOW}⚠ could not parse the intranet API Server endpoint from the kubeconfig${NC}"
		return 1
	fi
	APISERVER_REMOTE_HOSTPORT="${host}:${port}"

	echo -e "  ${CYAN}Opening intranet API Server tunnel via jumpserver...${NC}"
	if ! _start_apiserver_tunnel; then
		echo -e "  ${YELLOW}⚠ failed to open the API Server tunnel through the jumpserver${NC}"
		return 1
	fi
	echo -e "  ${GREEN}✓ Intranet API Server tunnel ready: 127.0.0.1:${APISERVER_LOCAL_PORT} → ${host}:${port}${NC}"
	return 0
}

# ---------------------------------------------------------------
# _wait_tke_api_server — poll the TKE API Server until it answers (or time out).
#   Returns 0 when ready, non-zero on timeout. Required before STEP 3 deploys the
#   k8s addons in the same run that created the cluster: the kubernetes provider
#   must be able to reach the API Server during plan/apply. The apiserver is
#   intranet-only, so the probe goes through the jumpserver tunnel
#   (APISERVER_PROBE_URL set by _open_apiserver_tunnel).
# ---------------------------------------------------------------
_wait_tke_api_server() {
	echo -e "  ${CYAN}Waiting for the TKE API Server to be ready (via intranet tunnel)...${NC}"
	local probe i http_status _kc _refreshed=0
	for i in $(seq 1 30); do
		if [ -z "${APISERVER_REMOTE_HOSTPORT}" ]; then
			# Endpoint not derived yet: (re)write the local kubeconfig from the
			# intranet output (refreshing it if the cluster output still lags) and
			# open the tunnel from it.
			_kc=$(terraform output -raw tke_kube_config 2>/dev/null || echo "")
			# `terraform refresh` hits many cloud APIs and can take tens of
			# seconds; do it at most once (the kubeconfig only needs to be pulled
			# into state a single time) and otherwise just re-read the output.
			if ! echo "$_kc" | grep -q '^apiVersion' && [ "$_refreshed" -eq 0 ]; then
				terraform refresh >/dev/null 2>&1 || true
				_refreshed=1
				_kc=$(terraform output -raw tke_kube_config 2>/dev/null || echo "")
			fi
			if echo "$_kc" | grep -q '^apiVersion'; then
				printf '%s' "$_kc" >"${SCRIPT_DIR}/.kube/config"
				chmod 600 "${SCRIPT_DIR}/.kube/config" 2>/dev/null || true
				_open_apiserver_tunnel || true
			fi
		elif [ -z "${APISERVER_TUNNEL_PID}" ] || ! kill -0 "${APISERVER_TUNNEL_PID}" 2>/dev/null; then
			# Re-establish the tunnel if it dropped.
			_start_apiserver_tunnel || true
		fi
		probe="${APISERVER_PROBE_URL}"
		if [ -n "$probe" ]; then
			http_status=$(curl -sk --connect-timeout 10 -o /dev/null -w "%{http_code}" \
				"${probe}/api/v1/namespaces" 2>/dev/null || echo "000")
			if [ "$http_status" = "200" ] || [ "$http_status" = "403" ]; then
				echo -e "  ${GREEN}✓ TKE API Server ready (HTTP ${http_status}) via intranet tunnel${NC}"
				return 0
			fi
			echo -ne "\r  ${CYAN}Attempt $i/30: HTTP ${http_status}, retrying...${NC}"
		fi
		sleep 10
	done
	echo ""
	return 1
}

# ---------------------------------------------------------------
# print_cluster_operator_help — at the very end of a cluster-edition (TKE)
#   deployment, print the practical information an operator needs to drive the
#   cluster by hand: how to log in to the jumpserver, every CLB IP and its port,
#   the Web UI URL, and how to control which ports each IP exposes to the public.
#   Skipped when no TKE cluster exists (single-node/CVM-only deployments).
# ---------------------------------------------------------------
print_cluster_operator_help() {
	local tke_id
	tke_id=$(terraform output -raw tke_cluster_id 2>/dev/null || echo "")
	# CLB IPs only exist for the TKE (cluster edition) deployment.
	[ -z "$tke_id" ] && return 0

	local key_file js_ip clb_sg_id
	key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"
	js_ip=$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "")
	clb_sg_id=$(terraform output -json security_group_ids 2>/dev/null | jq -r '.clb // empty' 2>/dev/null || echo "")

	local webui_ip proxy_ip api_ip master_ip
	webui_ip=$(terraform output -raw tke_cube_webui_clb_ip 2>/dev/null || echo "")
	proxy_ip=$(terraform output -raw tke_cube_proxy_clb_ip 2>/dev/null || echo "")
	api_ip=$(terraform output -raw tke_cube_api_clb_ip 2>/dev/null || echo "")
	master_ip=$(terraform output -raw tke_cubemaster_clb_ip 2>/dev/null || echo "")

	echo ""
	_draw_box "${GREEN}" \
		"CubeSandbox cluster edition — operator guide" \
		"Everything below is what you need to drive the cluster by hand."

	# 1) Jumpserver login
	echo ""
	echo -e "${CYAN}▶ 1. Log in to the jumpserver (SSH runs on port 443)${NC}"
	if [ -n "$js_ip" ]; then
		echo -e "    ssh -i ${key_file} -p 443 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null root@${js_ip}"
		echo -e "    ${YELLOW}The jumpserver sits in the VPC and already holds:${NC}"
		echo -e "      • kubectl + kubeconfig at /root/.kube/config (kubectl get pods -n cubesandbox)"
		echo -e "      • the cubemastercli tool (cubemastercli --help)"
		echo -e "      • the SSH key to reach the internal compute nodes"
	else
		echo -e "    ${YELLOW}Jumpserver public IP unavailable (check: terraform output jumpserver_public_ip)${NC}"
	fi

	# 2) CLB IPs and their ports
	echo ""
	echo -e "${CYAN}▶ 2. CLB (load balancer) IPs and ports${NC}"
	echo -e "    ${GREEN}cube-webui${NC}  (public, HTTP)   : ${webui_ip:-N/A}  → port 80"
	echo -e "    ${GREEN}cube-proxy${NC}  (public, TCP)    : ${proxy_ip:-N/A}  → ports 80, 443"
	echo -e "    ${GREEN}cube-api${NC}    (public, TCP)    : ${api_ip:-N/A}  → port 3000"
	echo -e "    ${GREEN}cube-master${NC} (VPC-internal)   : ${master_ip:-N/A}  → port 8089 (reachable from the jumpserver/VPC only)"

	# 3) Web UI
	echo ""
	echo -e "${CYAN}▶ 3. Web UI${NC}"
	if [ -n "$webui_ip" ]; then
		echo -e "    Open in your browser:  ${GREEN}http://${webui_ip}/${NC}"
		echo -e "    The Web UI talks to cube-api; if you front it with your own"
		echo -e "    domain/TLS, point it at this CLB IP (port 80)."
	else
		echo -e "    ${YELLOW}Web UI CLB IP not ready yet. It can take a minute after the addons deploy.${NC}"
		echo -e "    ${YELLOW}Check again: terraform output tke_cube_webui_clb_ip${NC}"
	fi

	# 4) Controlling which ports are exposed publicly
	echo ""
	echo -e "${CYAN}▶ 4. Control which ports each IP exposes to the internet${NC}"
	echo -e "    The deployment uses 4 per-role security groups (least privilege):"
	echo -e "      ${GREEN}cubesandbox-sg-jumpserver${NC} : jumpserver SSH 443 + VPC internal"
	echo -e "      ${GREEN}cubesandbox-sg-compute${NC}    : TKE pod CIDR + VPC internal only (no public ingress)"
	echo -e "      ${GREEN}cubesandbox-sg-tke-pod${NC}    : pod-to-pod + VPC internal only (no public ingress)"
	if [ "${TF_VAR_enable_public_network:-false}" = "true" ]; then
		echo -e "      ${GREEN}cubesandbox-sg-clb${NC}        : public service ports for the CLBs below"
		echo -e "    All CLBs above share the CLB security group:"
		echo -e "      ${GREEN}${clb_sg_id:-cubesandbox-sg-clb}${NC} (name: cubesandbox-sg-clb)"
		echo -e "    Its inbound rules open to 0.0.0.0/0 (the whole internet):"
		echo -e "      • 80   → cube-webui + cube-proxy (HTTP)"
		echo -e "      • 443  → cube-proxy (HTTPS)"
		echo -e "      • 3000 → cube-api"
		echo -e "    VPC-internal only (not reachable from the internet):"
		echo -e "      • 8089 → cube-master (internal CLB)"
	else
		echo -e "      ${GREEN}cubesandbox-sg-clb${NC}        : VPC-internal service ports for the CLBs below"
		echo -e "    All CLBs above share the CLB security group:"
		echo -e "      ${GREEN}${clb_sg_id:-cubesandbox-sg-clb}${NC} (name: cubesandbox-sg-clb)"
		echo -e "    Its inbound rules are scoped to the VPC CIDR 10.0.0.0/16 (no public exposure):"
		echo -e "      • 80   → cube-webui + cube-proxy (HTTP)"
		echo -e "      • 443  → cube-proxy (HTTPS)"
		echo -e "      • 3000 → cube-api"
		echo -e "      • 8089 → cube-master"
	fi
	echo -e "    Jumpserver SSH 443 lives on cubesandbox-sg-jumpserver, not the CLB group."
	echo ""
}

# ---------------------------------------------------------------
# wait_jumpserver_ready — wait until the jumpserver SSH (443) is reachable and a
#   login succeeds, then upload the SSH key, verify/sync the bundle (md5) and
#   install cubemastercli. Returns 0 on success, 1 on failure (the caller decides
#   whether to abort). Used both early (existing jumpserver, before the slow apply)
#   and after the apply (first creation).
# ---------------------------------------------------------------
wait_jumpserver_ready() {
	local js_public_ip js_private_ip key_file i
	js_public_ip=$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "")
	js_private_ip=$(terraform output -raw jumpserver_private_ip 2>/dev/null || echo "")
	key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"
	[ -z "$js_public_ip" ] && {
		echo -e "${RED}✗ jumpserver public IP unavailable${NC}"
		return 1
	}

	echo -e "  ${YELLOW}jumpserver public IP: ${js_public_ip}${NC}"
	echo -e "  ${YELLOW}jumpserver internal IP: ${js_private_ip}${NC}"
	echo -e "  ${YELLOW}SSH: ssh -i ${key_file} -p 443 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null root@${js_public_ip}${NC}"
	echo ""

	# Wait for the jumpserver SSH to be ready by probing with SSH itself — the real
	# test — instead of `nc -z`, whose `-z` flag is not portable across netcat
	# variants (ncat/GNU netcat) and often always "fails" even when the port is
	# open, causing a needless long wait. The jumpserver runs cloud-init on first
	# boot (moving SSH to 443, installing tools), so retry for up to ~10 minutes.
	echo -n "  Waiting for the jumpserver SSH to be ready (${js_public_ip}:443)..."
	local max_wait="${TENCENTCLOUD_JUMPSERVER_SSH_WAIT:-200}"
	local ssh_ok=0 ssh_out
	for i in $(seq 1 "$max_wait"); do
		ssh_out=$(ssh -i "${key_file}" -p 443 \
			-o StrictHostKeyChecking=no \
			-o UserKnownHostsFile=/dev/null \
			-o ConnectTimeout=5 \
			-o BatchMode=yes \
			-o LogLevel=ERROR \
			root@"${js_public_ip}" \
			"echo SSH_OK" 2>&1) || true
		if echo "$ssh_out" | grep -q "SSH_OK"; then
			ssh_ok=1
			echo ""
			break
		fi
		echo -n "."
		sleep 3
	done

	if [ "$ssh_ok" -ne 1 ]; then
		echo ""
		echo -e "${RED}✗ jumpserver SSH not ready (timed out)${NC}"
		return 1
	fi

	echo -e "${GREEN}✓ jumpserver SSH is ready${NC}"
	# Upload the private key to the jumpserver so it can SSH/SCP to internal nodes
	_setup_jumpserver_key
	# Every run: make sure the jumpserver's bundle tar.gz exists and its md5 matches
	# the local bundle (re-upload otherwise), before anything consumes it.
	ensure_js_bundle || echo -e "  ${YELLOW}⚠ Bundle verification/upload had issues; later steps will retry.${NC}"
	# Install the cubemastercli management tool on the jumpserver
	_install_cubemastercli
	return 0
}

# ---------------------------------------------------------------
# _reconcile_addons — before the STEP 3 terraform apply, make the cluster match
#   what terraform is about to create, via the jumpserver's kubectl:
#     • ALWAYS delete the four component Deployments, so they are recreated with
#       the freshly built/pushed images (the user's "delete then redeploy" intent).
#     • Delete a Service/ConfigMap/Secret ONLY when it exists in the cluster but
#       is MISSING from terraform state (state drift). Such drift makes the apply
#       fail with "... already exists"; deleting the drifted object lets terraform
#       recreate it. In-state objects are left untouched so stable Services/CLBs
#       are not needlessly churned.
#   No-op on a first creation (namespace/objects don't exist yet).
# ---------------------------------------------------------------
_reconcile_addons() {
	_js_kubectl get ns cubesandbox 2>/dev/null | grep -q Active || return 0

	local in_state
	in_state="$(terraform state list 2>/dev/null | grep -E '^kubernetes_' || true)"

	# <terraform address>|<kubectl delete args>
	local entries='
kubernetes_secret.cube_egress_ca|-n cubesandbox delete secret cube-egress-ca
kubernetes_secret.cubemaster_conf|-n cubesandbox delete secret cubemaster-conf
kubernetes_secret.cube_lifecycle_manager_conf|-n cubesandbox delete secret cube-lifecycle-manager-conf
kubernetes_secret.cubeproxy_global|-n cubesandbox delete secret cubeproxy-global
kubernetes_secret.cubeproxy_certs|-n cubesandbox delete secret cubeproxy-certs
kubernetes_config_map.cubeproxy_nginx_conf|-n cubesandbox delete configmap cubeproxy-nginx-conf
kubernetes_config_map.cube_webui_nginx_conf|-n cubesandbox delete configmap cube-webui-nginx-conf
kubernetes_service.cubemaster|-n cubesandbox delete svc cubemaster
kubernetes_service.cube_api|-n cubesandbox delete svc cube-api
kubernetes_service.cube_lifecycle_manager|-n cubesandbox delete svc cube-lifecycle-manager
kubernetes_service.cube_proxy|-n cubesandbox delete svc cube-proxy
kubernetes_service.cube_webui|-n cubesandbox delete svc cube-webui
kubernetes_deployment.cubemaster|-n cubesandbox delete deploy cubemaster
kubernetes_deployment.cube_api|-n cubesandbox delete deploy cube-api
kubernetes_deployment.cube_lifecycle_manager|-n cubesandbox delete deploy cube-lifecycle-manager
kubernetes_deployment.cube_proxy|-n cubesandbox delete deploy cube-proxy
kubernetes_deployment.cube_webui|-n cubesandbox delete deploy cube-webui
'

	echo -e "  ${CYAN}Reconciling cluster vs. terraform state before redeploy...${NC}"
	local addr args
	while IFS='|' read -r addr args; do
		[ -z "$addr" ] && continue
		case "$addr" in
		kubernetes_deployment.*)
			# Always delete Deployments → recreated with the freshly built images.
			;;
		*)
			# Others: only delete when drifted (missing from state) to avoid
			# churning stable Services/CLBs that terraform can update in place.
			if echo "$in_state" | grep -qF "$addr"; then
				continue
			fi
			echo -e "  ${YELLOW}  drift: ${addr} exists in cluster but not in state → deleting${NC}"
			;;
		esac
		# shellcheck disable=SC2086
		_js_kubectl ${args} --ignore-not-found 2>/dev/null || true
	done <<EOF
${entries}
EOF
	echo -e "  ${GREEN}✓ Reconciliation done; terraform will (re)create the Deployments${NC}"
}

# ---------------------------------------------------------------
# phase7_health_check — synchronously verify the cluster components are healthy
#   after the TKE addons have been deployed: wait for the cube-master, cube-api,
#   cube-lifecycle-manager, cube-proxy and cube-webui Deployments to roll out, then probe the HTTP
#   health endpoints of cube-master and cube-api through their CLBs (reachable
#   from the jumpserver). Returns 0 only when every component is healthy, so the
#   orchestrator can fail-fast.
# ---------------------------------------------------------------
phase7_health_check() {
	banner "Step: Health check — cube-master / cube-api / cube-lifecycle-manager / cube-proxy / cube-webui"

	local ns="cubesandbox"
	# The namespace must be present (created by the addons apply in Step 6).
	if ! _js_kubectl get ns "${ns}" 2>/dev/null | grep -q Active; then
		echo -e "  ${RED}✗ namespace ${ns} not found (addons not deployed?)${NC}"
		return 1
	fi

	# ---- Cluster snapshot (Deployments / Pods / Services) -------------------
	echo -e "  ${CYAN}── Deployments ──${NC}"
	_js_kubectl -n "${ns}" get deploy -o wide 2>&1 | sed 's/^/    /'
	echo -e "  ${CYAN}── Pods ──${NC}"
	_js_kubectl -n "${ns}" get pods -o wide 2>&1 | sed 's/^/    /'
	echo -e "  ${CYAN}── Services (CLB) ──${NC}"
	_js_kubectl -n "${ns}" get svc -o wide 2>&1 | sed 's/^/    /'
	echo ""

	# ---- 1) Wait for each Deployment to roll out (synchronous, fail-fast) ---
	#     On failure, dump pod state + events + container logs to explain why.
	local dep out ready ok=1
	for dep in cubemaster cube-api cube-lifecycle-manager cube-proxy cube-webui; do
		echo -e "  ${CYAN}▶ deployment/${dep}: waiting for rollout (timeout 300s)...${NC}"
		out=$(_js_kubectl -n "${ns}" rollout status deploy/"${dep}" --timeout=300s 2>&1)
		if echo "$out" | grep -qi "successfully rolled out"; then
			ready=$(_js_kubectl -n "${ns}" get deploy "${dep}" -o jsonpath='{.status.readyReplicas}/{.spec.replicas}' 2>/dev/null)
			echo -e "    ${GREEN}✓ ${dep} rolled out (ready replicas: ${ready:-?})${NC}"
		else
			echo -e "    ${RED}✗ ${dep} did not become available:${NC}"
			echo "$out" | tail -n 3 | sed 's/^/        /'
			echo -e "    ${YELLOW}pods:${NC}"
			_js_kubectl -n "${ns}" get pods -l app="${dep}" -o wide 2>&1 | sed 's/^/        /'
			echo -e "    ${YELLOW}recent events:${NC}"
			_js_kubectl -n "${ns}" describe deploy "${dep}" 2>&1 | sed -n '/Events:/,$p' | head -n 12 | sed 's/^/        /'
			echo -e "    ${YELLOW}container logs (tail):${NC}"
			_js_kubectl -n "${ns}" logs deploy/"${dep}" --tail=20 --all-containers 2>&1 | sed 's/^/        /'
			ok=0
		fi
	done
	if [ "$ok" != "1" ]; then
		return 1
	fi
	echo ""

	# ---- 2) Probe the component endpoints through the CLBs (from jumpserver) -
	local cm_ip api_ip proxy_ip webui_ip
	cm_ip=$(terraform output -raw tke_cubemaster_clb_ip 2>/dev/null || echo "")
	api_ip=$(terraform output -raw tke_cube_api_clb_ip 2>/dev/null || echo "")
	proxy_ip=$(terraform output -raw tke_cube_proxy_clb_ip 2>/dev/null || echo "")
	webui_ip=$(terraform output -raw tke_cube_webui_clb_ip 2>/dev/null || echo "")

	# cube-master /notify/health on 8089 (VPC-internal CLB)
	if [ -n "$cm_ip" ]; then
		echo -e "  ${CYAN}▶ cube-master  GET http://${cm_ip}:8089/notify/health${NC}"
		if _http_probe "http://${cm_ip}:8089/notify/health"; then
			local cm_body
			cm_body=$(_jump_exec "curl -s --connect-timeout 5 --max-time 10 'http://${cm_ip}:8089/notify/health' 2>/dev/null" 2>/dev/null)
			echo -e "    ${GREEN}✓ healthy${NC} (HTTP ${_HTTP_PROBE_CODE}) response: ${cm_body:0:120}"
		else
			echo -e "    ${RED}✗ cube-master health check failed (last HTTP ${_HTTP_PROBE_CODE:-N/A})${NC}"
			return 1
		fi
		# Informational: how many compute nodes have registered so far. The
		# standalone compute nodes only register in Step 8, so 0 here is normal.
		local nodes_json ncount
		nodes_json=$(_jump_exec "curl -s --connect-timeout 5 --max-time 10 'http://${cm_ip}:8089/internal/meta/nodes' 2>/dev/null" 2>/dev/null)
		ncount=$(echo "$nodes_json" | jq -r '.data | length' 2>/dev/null || echo "0")
		echo -e "    ${CYAN}registered compute nodes so far: ${ncount:-0} (they register in Step 8)${NC}"
	else
		echo -e "  ${RED}✗ cube-master CLB IP not available${NC}"
		return 1
	fi

	# cube-api /health on 3000 (public CLB, also reachable from the jumpserver)
	if [ -n "$api_ip" ]; then
		echo -e "  ${CYAN}▶ cube-api  GET http://${api_ip}:3000/health${NC}"
		if _http_probe "http://${api_ip}:3000/health"; then
			local api_body
			api_body=$(_jump_exec "curl -s --connect-timeout 5 --max-time 10 'http://${api_ip}:3000/health' 2>/dev/null" 2>/dev/null)
			echo -e "    ${GREEN}✓ healthy${NC} (HTTP ${_HTTP_PROBE_CODE}) response: ${api_body:0:120}"
		else
			echo -e "    ${RED}✗ cube-api health check failed (last HTTP ${_HTTP_PROBE_CODE:-N/A})${NC}"
			return 1
		fi
	else
		echo -e "  ${RED}✗ cube-api CLB IP not available${NC}"
		return 1
	fi

	# cube-proxy is a TCP proxy (no plain HTTP health route); confirm its CLB
	# answers a TCP connection on port 80.
	if [ -n "$proxy_ip" ]; then
		echo -e "  ${CYAN}▶ cube-proxy  TCP ${proxy_ip}:80${NC}"
		local i tcp_ok=0
		for i in $(seq 1 30); do
			if _jump_exec "timeout 5 bash -c '</dev/tcp/${proxy_ip}/80' 2>/dev/null && echo TCP_OK" 2>/dev/null | grep -q TCP_OK; then
				tcp_ok=1
				break
			fi
			echo -ne "\r    ${CYAN}attempt ${i}/30: connecting...${NC}"
			sleep 5
		done
		echo ""
		if [ "$tcp_ok" = "1" ]; then
			echo -e "    ${GREEN}✓ cube-proxy reachable on ${proxy_ip}:80${NC}"
		else
			echo -e "    ${RED}✗ cube-proxy not reachable on ${proxy_ip}:80${NC}"
			return 1
		fi
	else
		echo -e "  ${RED}✗ cube-proxy CLB IP not available${NC}"
		return 1
	fi

	# cube-webui (informational): probe the public HTTP root.
	if [ -n "$webui_ip" ]; then
		echo -e "  ${CYAN}▶ cube-webui  GET http://${webui_ip}/ ${NC}"
		if _http_probe "http://${webui_ip}/"; then
			echo -e "    ${GREEN}✓ cube-webui reachable${NC} (HTTP ${_HTTP_PROBE_CODE}) → open http://${webui_ip}/"
		else
			echo -e "    ${YELLOW}⚠ cube-webui not answering yet (HTTP ${_HTTP_PROBE_CODE:-N/A}); the CLB may still be warming up${NC}"
		fi
	fi

	echo ""
	echo -e "  ${GREEN}✓ Component health checks passed${NC}"
	echo -e "  ${CYAN}Summary:${NC}"
	echo -e "    cube-master : ${cm_ip:-N/A}:8089   (VPC-internal)"
	echo -e "    cube-api    : ${api_ip:-N/A}:3000  (public)"
	echo -e "    cube-proxy  : ${proxy_ip:-N/A}:80/443 (public)"
	echo -e "    cube-webui  : ${webui_ip:-N/A}:80   (public)"
	return 0
}

# _http_probe — poll an HTTP endpoint (from the jumpserver) until it returns
#   200/403 or a retry budget is exhausted. Sets _HTTP_PROBE_CODE to the last
#   observed HTTP status. Returns 0 on success.
_HTTP_PROBE_CODE=""
_http_probe() {
	local url="$1" i code
	_HTTP_PROBE_CODE=""
	for i in $(seq 1 30); do
		code=$(_jump_exec "curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 10 '${url}' 2>/dev/null" 2>/dev/null)
		code=$(echo "$code" | tr -dc '0-9')
		_HTTP_PROBE_CODE="${code:-N/A}"
		if [ "${code:-0}" = "200" ] || [ "${code:-0}" = "403" ]; then
			return 0
		fi
		echo -ne "\r    ${CYAN}attempt ${i}/30: HTTP ${code:-N/A}...${NC}"
		sleep 5
	done
	echo ""
	return 1
}

main() {
	echo ""
	_draw_box "${GREEN}" "CubeSandbox Cluster on Tencent Cloud"

	# 0. Environment variable processing
	# Make sure the terraform CLI exists before anything else needs it
	ensure_terraform
	# jq is a hard dependency for parsing `terraform output -json` (compute node
	# IPs, etc.); install it now so later steps don't silently misread 0/empty.
	ensure_jq
	# Preload previously saved selections (if any) so a re-create after destroy
	# reuses the same configuration. Explicit environment variables still win.
	load_saved_env
	# Capture whether the image build/tag were already configured (via env vars or
	# the saved selection just loaded) BEFORE prompt_deployment_env resolves them,
	# so tcr_build_and_push can skip the interactive "build & push?" confirmation
	# and just remind when the deployment is env-driven.
	IMAGES_CONFIGURED=0
	if [ -n "${TENCENTCLOUD_BUILD_IMAGES:-}" ] || [ -n "${TENCENTCLOUD_CUBE_IMAGE_TAG:-}" ]; then
		IMAGES_CONFIGURED=1
	fi
	check_credentials
	# Surface every deployment-config env var: when unset, the user confirms the
	# default or types a custom value. Runs before setup_env so its values feed
	# the TENCENTCLOUD_* → TF_VAR_* mapping (e.g. region must be set before plan).
	prompt_deployment_env
	setup_env
	load_resolved_tfvars_file
	_setup_jump_proxy

	# 1. Initialize terraform
	step1_init

	# tke-addons.tf renders nginx configs via file(), which Terraform evaluates on
	# EVERY plan/apply regardless of resource count. The files must therefore exist
	# before the FIRST terraform plan. Release bundles already ship them; this also
	# covers running create.sh straight from the source tree.
	prepare_webui_nginx_conf
	prepare_cubeproxy_nginx_conf

	# 1.1 Reconcile stale Kubernetes state: if the TKE cluster is NOT in state (it
	# was destroyed / never created / removed from state) but leftover kubernetes_*
	# resources remain, the kubernetes provider has no cluster to reach and every
	# subsequent plan/apply fails with: Get "http://localhost/..." connection
	# refused. Drop those stale resources from state so the flow can proceed; STEP 3
	# recreates them once the cluster exists again.
	local _state_list
	_state_list="$(terraform state list 2>/dev/null || true)"
	if phase_should_prune_stale_k8s "$_state_list"; then
		while IFS= read -r _k8s_addr; do
			[ -n "$_k8s_addr" ] || continue
			echo -e "  ${YELLOW}Pruning stale ${_k8s_addr} from state (no live TKE cluster)${NC}"
			terraform state rm "$_k8s_addr" >/dev/null 2>&1 || true
		done < <(printf '%s\n' "$_state_list" | grep -E '^kubernetes_' || true)
	fi

	# 2. Interactive selection (if environment variables are not set)
	select_zone
	select_instance_type
	select_compute_nodes
	select_tke
	_init_cvm_zones
	[ "${#JUMPSERVER_TYPES[@]}" -eq 0 ] && _fallback_jumpserver_instance_types

	local saved_compute_count="${TF_VAR_compute_node_count:-0}"

	# Persist the resolved selections now, BEFORE the long provisioning steps, so
	# that re-running create.sh (before a destroy) reuses the exact same choices
	# even if a later step fails partway. The final save_env_file at the end
	# refreshes this with any values resolved later (e.g. vmlinux-pvm). load_saved_env
	# only fills unset values, so an explicit env override still wins.
	save_env_file quiet

	# Reconcile state with the real environment BEFORE the provisioning
	# applies: refresh out-of-band attribute changes and import any
	# stateful resources that exist in the cloud but are missing from the
	# local state, so the applies below don't collide with them
	# ("... already exists") or work off stale attributes. Best-effort and
	# a no-op on a first creation (empty state / no jumpserver yet).
	ss_sync_state || true
	_sync_compute_config_from_state

	# ============================================================
	# Sequential, fail-fast provisioning. Each step below is a synchronous
	# terraform apply (restricted to that step's resources via -target) that must
	# finish before the next one starts; any failure aborts the whole deployment.
	# The TKE cluster + addons are created LAST (Step 6) so the kubernetes provider
	# only connects once the API Server exists — until then create_tke /
	# deploy_tke_addons stay OFF for every base apply.
	#
	# This is the deployment state machine, and it must stay recoverable across
	# reruns / partial failures. The high-risk decisions (phase flags, never
	# scaling compute down, reusing an existing cluster, pruning stale k8s state)
	# are isolated as pure functions in lib-phases.sh and covered by
	# deploy/one-click/tests/test_phase_flags.sh — a cloud-free dry run of the
	# first-run, rerun and partial-failure transitions. Keep that coverage in sync
	# when changing the flow below.
	# ============================================================
	_set_phase_flags "$(phase_base_flags)"

	# Keep existing compute nodes on a re-run (never scale down): the effective
	# count is max(desired, already-in-state) so a partial/rerun never destroys
	# compute nodes that were already purchased (see phase_effective_compute_count).
	local existing_count effective_count
	# Anchor + escape the address (matching the stricter count used later) so only
	# real tencentcloud_instance.compute[i] entries are counted — an unanchored
	# 'tencentcloud_instance.compute' would also match e.g. a data.* address and
	# over-count, feeding a wrong "existing" into phase_effective_compute_count.
	existing_count=$(terraform state list 2>/dev/null | grep -c '^tencentcloud_instance\.compute\[' 2>/dev/null) || existing_count=0
	effective_count=$(phase_effective_compute_count "$saved_compute_count" "$existing_count")
	if [ "$effective_count" -gt "$saved_compute_count" ] 2>/dev/null; then
		echo -e "  ${YELLOW}⚠ ${existing_count} compute node(s) already exist; keeping them (no scale-down)${NC}"
	fi
	saved_compute_count="$effective_count"
	export TF_VAR_compute_node_count="$saved_compute_count"

	# ============================================================
	# Step 1/9 — Subnet + NAT gateway (network foundation)
	#   Pulls in the VPC, EIP, security group + rules and the SSH key pair as
	#   dependencies; everything else attaches to this network.
	# ============================================================
	_apply_phase "Step: Configure subnet + NAT gateway" \
		tencentcloud_subnet.cluster \
		tencentcloud_nat_gateway.cluster \
		tencentcloud_route_table_entry.nat \
		tencentcloud_security_group_rule_set.jumpserver \
		tencentcloud_security_group_rule_set.compute \
		tencentcloud_security_group_rule_set.tke_pod \
		tencentcloud_security_group_rule_set.clb \
		tencentcloud_key_pair.cluster || {
		echo -e "${RED}✗ Network provisioning failed; aborting deployment.${NC}"
		exit 1
	}

	# ============================================================
	# Step 2/9 — TCR (container registry) service, optional
	# ============================================================
	if [ "${TF_VAR_use_tcr:-false}" = "true" ]; then
		_apply_phase "Step: Configure TCR service" \
			tencentcloud_tcr_vpc_attachment.cluster[0] \
			tencentcloud_tcr_namespace.cluster[0] \
			tencentcloud_tcr_token.cluster[0] || {
			echo -e "${RED}✗ TCR provisioning failed; aborting deployment.${NC}"
			exit 1
		}
	else
		echo -e "${GREEN}✓ TCR disabled; using public component images.${NC}"
	fi

	# ============================================================
	# Step 3/9 — Purchase CVMs: jump-server + compute nodes
	#   step2_apply auto-fallback cycles instance types and per-role zones
	#   (same VPC, different subnets) when a type is out of stock.
	# ============================================================
	STEP2_LABEL="Step: Purchase jump-server"
	STEP2_TARGETS=(tencentcloud_instance.jumpserver)
	STEP2_CVM_FALLBACK=1
	step2_apply

	STEP2_LABEL="Step: Purchase compute nodes"
	purchase_compute_nodes "${saved_compute_count:-0}"
	STEP2_LABEL="Step: terraform apply (create CVM)"

	# Verify the compute CVMs were actually created (fail-fast): the deployment
	# needs them, and a silent miss here would only surface as a confusing
	# "no compute nodes" message much later in Step 8.
	if [ "${saved_compute_count:-0}" -gt 0 ] 2>/dev/null; then
		local _compute_created
		_compute_created=$(terraform state list 2>/dev/null | grep -c '^tencentcloud_instance\.compute\[' 2>/dev/null) || _compute_created=0
		echo -e "  ${CYAN}Compute nodes in state: ${_compute_created} (desired ${saved_compute_count})${NC}"
		if [ "$_compute_created" -lt "$saved_compute_count" ] 2>/dev/null; then
			echo -e "${RED}✗ Step 3: expected ${saved_compute_count} compute node(s) but only ${_compute_created} were created.${NC}"
			echo -e "  ${YELLOW}Likely cause: instance-type stock — see the apply log above.${NC}"
			echo -e "  ${YELLOW}Fix: adjust TENCENTCLOUD_COMPUTE_INSTANCE_TYPE (preference) / zones and re-run.${NC}"
			exit 1
		fi
		echo -e "  ${GREEN}✓ ${_compute_created} compute node(s) created${NC}"
		local _actual_types
		_actual_types=$(terraform output -json compute_instance_types 2>/dev/null | jq -r 'join(", ")' 2>/dev/null || echo "")
		[ -n "$_actual_types" ] && echo -e "  ${CYAN}Actual compute cluster: ${_actual_types}${NC}"
	fi

	# ============================================================
	# Step 4/9 — jump-server initialization and optional TCR image build/push
	# ============================================================
	banner "Step: Initialize jump-server"
	wait_jumpserver_ready || {
		echo -e "${RED}✗ jump-server not reachable; aborting deployment.${NC}"
		exit 1
	}
	IMAGES_OK=1
	if [ "${TF_VAR_use_tcr:-false}" = "true" ]; then
		_apply_phase "Step: Deploy TCR token to jump-server" \
			null_resource.tcr_token_deploy[0] || {
			echo -e "${RED}✗ Failed to deploy the TCR token to the jump-server; aborting.${NC}"
			exit 1
		}
		tcr_build_and_push
		if [ "${IMAGES_OK}" != "1" ]; then
			echo -e "${RED}✗ Component image build/push failed; aborting deployment.${NC}"
			exit 1
		fi
	else
		echo -e "${GREEN}✓ Public images configured; skipping TCR token deploy and image build/push.${NC}"
	fi

	# ============================================================
	# Step 5/9 — MySQL + Redis
	#   Create the MySQL instance (+ cube account/privilege + the application
	#   database via the jump-server) and the Redis instance, then verify Redis is
	#   reachable. Database/account names come from var.cube_db / var.cube_user.
	# ============================================================
	if [ "${RESET_DB:-0}" = "1" ] || [ "${RESET_DB:-0}" = "true" ]; then
		local _reset_mysql_ip _reset_js_ip _reset_key _reset_pw
		_reset_mysql_ip=$(terraform output -raw mysql_intranet_ip 2>/dev/null || echo "")
		_reset_js_ip=$(terraform output -raw jumpserver_public_ip 2>/dev/null || echo "")
		_reset_key="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"
		_reset_pw="${TF_VAR_mysql_root_password:-CubeSandbox123!}"
		if [ -n "$_reset_mysql_ip" ] && [ -n "$_reset_js_ip" ]; then
			# CUBE_DB is validated to a bare SQL identifier (TF_VAR_cube_db has the
			# same regex validation), so it is safe to inline into the statement.
			echo -e "  ${YELLOW}⚠ RESET_DB=1, dropping the ${CUBE_DB:-cube_mvp} database...${NC}"
			# Feed the root password over stdin (read back by $(cat) on the remote)
			# so it is exported as MYSQL_PWD in the remote shell and never appears
			# on the remote process argv / `ps` output (CWE-214).
			printf '%s' "${_reset_pw}" | ssh -i "${_reset_key}" -p 443 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
				-o ConnectTimeout=10 -o BatchMode=yes -o LogLevel=ERROR root@"${_reset_js_ip}" \
				"set +H; MYSQL_PWD=\"\$(cat)\" mysql -h'${_reset_mysql_ip}' -P3306 -uroot -e 'DROP DATABASE IF EXISTS ${CUBE_DB:-cube_mvp}' 2>&1" 2>&1 || true
		fi
		terraform taint null_resource.mysql_init_db 2>/dev/null || true
	fi
	_apply_phase "Step: Configure MySQL + Redis" \
		tencentcloud_mysql_privilege.cube \
		tencentcloud_mysql_account.cube \
		null_resource.mysql_init_db \
		tencentcloud_redis_instance.redis || {
		echo -e "${RED}✗ MySQL/Redis provisioning failed; aborting deployment.${NC}"
		exit 1
	}
	_deploy_mysql || true
	_deploy_redis || true
	# Verify Redis connectivity (installs redis-cli on the jump-server and PINGs;
	# exits on failure for fail-fast).
	_init_redis

	# ============================================================
	# Step 5b/9 — CFS shared storage for cube-master, optional
	# ============================================================
	if [ "${TF_VAR_use_cfs:-false}" = "true" ]; then
		_apply_phase "Step: Configure CFS shared storage" \
			tencentcloud_cfs_access_rule.cubemaster_data[0] \
			tencentcloud_cfs_file_system.cubemaster_data[0] || {
			echo -e "${RED}✗ CFS provisioning failed; aborting deployment.${NC}"
			exit 1
		}
	else
		echo -e "${GREEN}✓ CFS disabled; cube-master uses pod-local emptyDir storage (single replica default).${NC}"
	fi

	# ============================================================
	# Step 6/9 — TKE cluster + addons
	#   6a) create the managed cluster + node pool (with instance-type fallback)
	#       and write the kubeconfig; deploy_tke_addons stays OFF so the kubernetes
	#       provider does not try to connect yet.
	#   6b) wait for the API Server, then apply the addons (cube-master / cube-api
	#       / cube-proxy / cube-webui) using the freshly built images.
	# ============================================================
	# TKE is always created. Turn create_tke back on for this final step (it was
	# kept OFF for the base applies above so the kubernetes provider would not
	# connect before the API Server existed).
	export TF_VAR_create_tke=true

	# 6a) Ensure the TKE cluster exists.
	#   terraform is declarative, so an EXISTING cluster in state is reused
	#   as-is (never re-purchased). The cluster apply is only run on a FIRST
	#   creation: it runs with deploy_tke_addons=false (no addons exist yet, so
	#   the kubernetes provider is not used) and a full apply also writes
	#   .kube/config via local_file.tke_kubeconfig.
	#   On a re-run we deliberately SKIP this apply: running it with
	#   deploy_tke_addons=false would drop the addon resources' count to 0 and
	#   TEAR DOWN the existing cube-master/api/proxy/webui Services (changing
	#   their CLB IPs). Instead the existing cluster is reused and the addons
	#   are reconciled in 6b.
	if phase_should_reuse_cluster "$(terraform state list 2>/dev/null || true)"; then
		echo -e "  ${GREEN}✓ Existing TKE cluster found in state — reusing it (no new cluster purchased)${NC}"
	else
		_set_phase_flags "$(phase_cluster_step_flags)"
		STEP2_LABEL="Step: Purchase TKE cluster"
		STEP2_TARGETS=(
			tencentcloud_kubernetes_cluster.tke[0]
			tencentcloud_kubernetes_node_pool.tke[0]
		)
		STEP2_CVM_FALLBACK=1
		step2_apply
		STEP2_TARGETS=()
		STEP2_CVM_FALLBACK=0
		STEP2_LABEL="Step: terraform apply (create CVM)"
	fi

	# Make sure the local kubeconfig used by the kubernetes provider is the
	# real one before the addons apply / kubectl calls below (it may still be
	# the placeholder, or the output may lag after a targeted apply). Write it
	# from the cluster's kube_config output.
	local _kc
	_kc=$(terraform output -raw tke_kube_config 2>/dev/null || echo "")
	if echo "$_kc" | grep -q '^apiVersion'; then
		mkdir -p "${SCRIPT_DIR}/.kube"
		printf '%s' "$_kc" >"${SCRIPT_DIR}/.kube/config"
		chmod 600 "${SCRIPT_DIR}/.kube/config" 2>/dev/null || true
	fi

	# The apiserver is intranet-only: open the jumpserver tunnel and point the
	# LOCAL kubeconfig at it, so the kubernetes provider (addons apply below)
	# and the API-server probe can reach the cluster from outside the VPC.
	_open_apiserver_tunnel || {
		echo -e "  ${RED}✗ Could not open the intranet API Server tunnel; aborting before addon deployment.${NC}"
		echo -e "  ${YELLOW}  Re-run after the jumpserver SSH(443) and TKE intranet endpoint are reachable.${NC}"
		exit 1
	}

	# 6b) Deploy the addons once the API Server answers.
	_wait_tke_api_server || {
		echo -e "  ${RED}✗ TKE API Server was not confirmed ready through the jumpserver tunnel; aborting before addon deployment.${NC}"
		exit 1
	}
	_localize_kubeconfig
	if ! grep -Eq "^[[:space:]]*server:[[:space:]]*https://127\\.0\\.0\\.1:${APISERVER_LOCAL_PORT}" "${SCRIPT_DIR}/.kube/config" 2>/dev/null; then
		echo -e "  ${RED}✗ Local kubeconfig is not pointing at the jumpserver tunnel; aborting before addon deployment.${NC}"
		echo -e "  ${YELLOW}  Expected server: https://127.0.0.1:${APISERVER_LOCAL_PORT}${NC}"
		echo -e "  ${YELLOW}  Current server: $(grep -E '^[[:space:]]*server:' "${SCRIPT_DIR}/.kube/config" 2>/dev/null | head -n1 || echo 'N/A')${NC}"
		exit 1
	fi
	# Upload the freshly written kubeconfig to the jump-server (for kubectl).
	_setup_jumpserver_key
	# addons prerequisite: the cube-proxy TLS certificate (webui-nginx.conf was
	# already generated right after terraform init, so the earlier plans/applies
	# could read it).
	# cube-proxy will CrashLoop (and terraform will drop the cubeproxy-certs
	# Secret) without both TLS files, so generate-or-BYO and then HARD require
	# them before touching the addons — fail fast instead of deploying a broken
	# cube-proxy in BUILD_IMAGES=0 / weak-network / incomplete-bundle runs.
	prepare_cubeproxy_certs || {
		echo -e "${RED}✗ cube-proxy TLS certificate preparation failed; aborting before addon deployment.${NC}"
		exit 1
	}
	_require_cubeproxy_certs || exit 1
	# Delete stale/drifted objects so the apply can (re)create the Deployments
	# with the freshly built images.
	_reconcile_addons
	_set_phase_flags "$(phase_addons_flags)"
	STEP2_LABEL="Step: Deploy TKE addons"
	STEP2_TARGETS=(
		kubernetes_namespace.cubesandbox[0]
		tls_private_key.cube_egress_ca[0]
		tls_self_signed_cert.cube_egress_ca[0]
		kubernetes_secret.cube_egress_ca[0]
		kubernetes_secret.cubemaster_conf[0]
		kubernetes_deployment.cubemaster[0]
		kubernetes_service.cubemaster[0]
		kubernetes_deployment.cube_api[0]
		kubernetes_service.cube_api[0]
		kubernetes_secret.cube_lifecycle_manager_conf[0]
		kubernetes_deployment.cube_lifecycle_manager[0]
		kubernetes_service.cube_lifecycle_manager[0]
		kubernetes_secret.cubeproxy_global[0]
		kubernetes_secret.cubeproxy_certs[0]
		kubernetes_config_map.cubeproxy_nginx_conf[0]
		kubernetes_deployment.cube_proxy[0]
		kubernetes_service.cube_proxy[0]
		kubernetes_config_map.cube_webui_nginx_conf[0]
		kubernetes_deployment.cube_webui[0]
		kubernetes_service.cube_webui[0]
	)
	STEP2_CVM_FALLBACK=0
	step2_apply || {
		echo -e "${RED}✗ TKE addons deployment failed; aborting deployment.${NC}"
		exit 1
	}
	STEP2_TARGETS=()
	STEP2_LABEL="Step: terraform apply (create CVM)"
	# The addons apply rewrote .kube/config with the raw intranet kubeconfig
	# (local_file.tke_kubeconfig); re-point it at the jumpserver tunnel so the
	# later terraform refresh / kubernetes provider keep reaching the cluster.
	_localize_kubeconfig
	# Restart the Deployments so any ConfigMap changes take effect.
	if _js_kubectl get ns cubesandbox 2>/dev/null | grep -q Active; then
		echo -e "  ${CYAN}Restarting Deployments...${NC}"
		for _dep in cubemaster cube-api cube-lifecycle-manager cube-proxy cube-webui; do
			_js_kubectl -n cubesandbox rollout restart deploy ${_dep} 2>/dev/null || true
		done
	fi

	# CLB IP summary
	echo -e "  ${CYAN}CLB IP summary:${NC}"
	local _cm_ip _api_ip _proxy_ip _webui_ip
	_cm_ip=$(terraform output -raw tke_cubemaster_clb_ip 2>/dev/null || echo "")
	_api_ip=$(terraform output -raw tke_cube_api_clb_ip 2>/dev/null || echo "")
	_proxy_ip=$(terraform output -raw tke_cube_proxy_clb_ip 2>/dev/null || echo "")
	_webui_ip=$(terraform output -raw tke_cube_webui_clb_ip 2>/dev/null || echo "")
	[ -n "$_cm_ip" ] && echo -e "    cubemaster: ${_cm_ip}"
	[ -n "$_api_ip" ] && echo -e "    cube-api:   ${_api_ip}"
	[ -n "$_proxy_ip" ] && echo -e "    cube-proxy: ${_proxy_ip}"
	[ -n "$_webui_ip" ] && echo -e "    cube-webui: ${_webui_ip}"
	echo ""

	# ============================================================
	# Step 7/9 — cube-master / cube-api / cube-proxy health check
	# ============================================================
	phase7_health_check || {
		echo -e "${RED}✗ Component health check failed; aborting deployment.${NC}"
		exit 1
	}

	# ============================================================
	# Step 8/9 — Compute node initialization + health check
	#   The compute CVMs were purchased in Step 3 (with instance-type fallback);
	#   here they get the PVM kernel + install-compute.sh, register with
	#   cube-master and are health-checked (node registration + template).
	# ============================================================
	banner "Step: Initialize compute nodes + health check"

	# Detect compute nodes authoritatively from terraform state (the
	# compute_private_ips OUTPUT can lag after the earlier targeted applies),
	# then read the private IPs from the output (refreshing them if they are
	# stale relative to the state). Always print the reason so it is never a
	# silent "nothing happened".
	compute_in_state=""
	compute_ips=""
	compute_count=""
	compute_in_state=$(terraform state list 2>/dev/null | grep -c '^tencentcloud_instance\.compute\[' 2>/dev/null) || compute_in_state=0
	compute_ips=$(terraform output -json compute_private_ips 2>/dev/null || echo "[]")
	compute_count=$(echo "$compute_ips" | jq -r 'length' 2>/dev/null || echo "0")
	if [ "$compute_in_state" -gt 0 ] && [ "${compute_count:-0}" -eq 0 ]; then
		echo -e "  ${YELLOW}Compute nodes exist in state but the output is stale; refreshing outputs...${NC}"
		terraform refresh >/dev/null 2>&1 || true
		compute_ips=$(terraform output -json compute_private_ips 2>/dev/null || echo "[]")
		compute_count=$(echo "$compute_ips" | jq -r 'length' 2>/dev/null || echo "0")
	fi

	echo -e "  ${CYAN}Compute nodes — desired: ${saved_compute_count:-0} · in terraform state: ${compute_in_state} · with private IP: ${compute_count:-0}${NC}"
	if [ "${compute_count:-0}" -gt 0 ]; then
		echo -e "  ${CYAN}Private IPs: $(echo "$compute_ips" | jq -r 'join(", ")' 2>/dev/null)${NC}"
	fi

	if [ "${compute_count:-0}" -gt 0 ] 2>/dev/null; then
		step8_init_compute_nodes || {
			echo -e "${RED}✗ One or more compute nodes failed to initialize; deployment incomplete.${NC}"
			exit 1
		}
	elif [ "${compute_in_state:-0}" -gt 0 ] 2>/dev/null; then
		# Compute CVMs exist, but their private IPs can't be read from the output.
		echo -e "${RED}✗ ${compute_in_state} compute node(s) exist in state but their private IPs could not be read.${NC}"
		echo -e "  ${YELLOW}The compute_private_ips output is empty even after a refresh. Try:${NC}"
		echo -e "  ${YELLOW}    terraform refresh && terraform output compute_private_ips${NC}"
		echo -e "  ${YELLOW}then re-run create.sh to finish compute-node initialization.${NC}"
		exit 1
	elif [ "${saved_compute_count:-0}" -gt 0 ] 2>/dev/null; then
		# Desired compute nodes but none were created — explain and fail-fast.
		echo -e "${RED}✗ Expected ${saved_compute_count} compute node(s) but none were created (none in terraform state).${NC}"
		echo -e "  ${YELLOW}They should have been created in Step 3. Likely cause: instance-type stock${NC}"
		echo -e "  ${YELLOW}in zone ${TF_VAR_availability_zone:-?}. Check the Step 3 apply log, adjust${NC}"
		echo -e "  ${YELLOW}TENCENTCLOUD_COMPUTE_INSTANCE_TYPE / TENCENTCLOUD_AVAILABILITY_ZONE, and re-run.${NC}"
		exit 1
	else
		echo -e "  ${CYAN}Compute node count is 0 (set TENCENTCLOUD_COMPUTE_NODE_COUNT to add nodes); nothing to initialize.${NC}"
	fi

	# ============================================================
	# Step 9/9 — Output usage / help information
	# ============================================================
	banner "Step: Deployment summary & usage information"

	# Deployment summary
	local key_file
	key_file="${TENCENTCLOUD_SSH_PRIVATE_KEY_PATH:-$SSH_PRI_KEY}"

	echo ""
	_draw_box "${GREEN}" "CubeSandbox deployment complete!"
	echo ""
	echo -e "  ${CYAN}CVM info:${NC}"
	echo -e "    Jumpserver public IP : $(terraform output -raw jumpserver_public_ip 2>/dev/null || echo N/A) (ssh -p 443 root@<ip>)"
	echo -e "    Compute node count    : ${saved_compute_count:-0}"
	echo -e "    Availability zone     : $(terraform output -json config_summary 2>/dev/null | jq -r .availability_zone 2>/dev/null || echo N/A)"
	echo -e "    Jumpserver zone       : $(terraform output -json config_summary 2>/dev/null | jq -r .jumpserver_availability_zone 2>/dev/null || echo N/A)"
	echo -e "    Compute preference    : $(terraform output -json config_summary 2>/dev/null | jq -r .compute_instance_type 2>/dev/null || echo N/A)"
	echo -e "    Compute cluster types : $(terraform output -json compute_instance_types 2>/dev/null | jq -r 'join(", ")' 2>/dev/null || echo N/A)"
	echo -e "    Compute cluster zones : $(terraform output -json compute_availability_zones 2>/dev/null | jq -r 'join(", ")' 2>/dev/null || echo N/A)"
	echo -e "    TKE worker zone       : $(terraform output -json config_summary 2>/dev/null | jq -r .tke_worker_availability_zone 2>/dev/null || echo N/A)"
	echo ""
	echo -e "  ${CYAN}SSH login:${NC}"
	echo -e "    ssh -i ${key_file} -p 443 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null root@\$(terraform output -raw jumpserver_public_ip)"
	echo ""

	# MySQL info
	mysql_ip=""
	mysql_ip=$(terraform output -raw mysql_intranet_ip 2>/dev/null || echo "")
	if [ -n "$mysql_ip" ]; then
		echo -e "  ${CYAN}MySQL:${NC}"
		echo -e "    Instance ID : $(terraform output -raw mysql_instance_id 2>/dev/null || echo N/A)"
		echo -e "    Internal IP : ${mysql_ip}:$(terraform output -raw mysql_intranet_port 2>/dev/null || echo 3306)"
		# Do not print the plaintext password to stdout (it would leak into
		# terminal scrollback / CI logs, CWE-532); it is persisted to the
		# 0600 .env instead.
		echo -e "    Password     : ${YELLOW}******** (saved in ${ENV_FILE})${NC}"
		echo ""
	fi

	# Redis info
	redis_ip=""
	redis_ip=$(terraform output -raw redis_intranet_ip 2>/dev/null || echo "")
	if [ -n "$redis_ip" ]; then
		echo -e "  ${CYAN}Redis:${NC}"
		echo -e "    Instance ID : $(terraform output -raw redis_instance_id 2>/dev/null || echo N/A)"
		echo -e "    Internal IP : ${redis_ip}:$(terraform output -raw redis_intranet_port 2>/dev/null || echo 6379)"
		# Masked on purpose (CWE-532); see the 0600 .env for the value.
		echo -e "    Password     : ${YELLOW}******** (saved in ${ENV_FILE})${NC}"
		echo ""
	fi

	# TKE info
	tke_id_info=""
	tke_id_info=$(terraform output -raw tke_cluster_id 2>/dev/null || echo "")
	if [ -n "$tke_id_info" ]; then
		tke_kc=""
		tke_kc=$(terraform output -raw tke_kube_config 2>/dev/null || echo "")
		echo -e "  ${CYAN}TKE:${NC}"
		echo -e "    Cluster ID : ${tke_id_info}"
		if [ -n "$tke_kc" ]; then
			echo -e "    Status     : Ready (kubeconfig available)"
			echo -e "    kubeconfig: uploaded to jumpserver:/root/.kube/config"
		else
			echo -e "    Status     : Configuring (kubeconfig available once the API Server is ready)"
			echo -e "    kubeconfig: uploaded to jumpserver:/root/.kube/config"
		fi
		echo ""
	fi

	# Print the operator guide for the cluster edition (jumpserver login, CLB IPs,
	# Web UI, and how to control which ports are exposed). No-op for CVM-only.
	print_cluster_operator_help

	# Final step: persist both the human selections and the actual resolved
	# Terraform values after fallback, so .env cannot drift from the deployed
	# resource shape.
	save_env_file
	write_resolved_tfvars_file
	echo ""
}

main "$@"

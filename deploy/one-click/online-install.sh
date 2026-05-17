#!/usr/bin/env bash
set -euo pipefail

GITHUB_REPO="tencentcloud/CubeSandbox"
GITHUB_API_BASE="https://api.github.com/repos/${GITHUB_REPO}"

CN_MIRROR_LATEST_URL="https://download.cubesandbox.com/release/latest.json"
MIRROR="${MIRROR:-}"

SKIP_PRECHECK="${ONE_CLICK_SKIP_PRECHECK:-0}"
DOWNLOAD_URL="${CUBE_SANDBOX_DOWNLOAD_URL:-}"
INSTALL_ARGS=()

for arg in "$@"; do
  case "${arg}" in
    --url=*) DOWNLOAD_URL="${arg#--url=}" ;;
    --skip-precheck) SKIP_PRECHECK=1 ;;
    *)       INSTALL_ARGS+=("${arg}") ;;
  esac
done

# ---------------------------------------------------------------------------
# Pre-download preflight checks (lightweight, self-contained)
# ---------------------------------------------------------------------------
check_early_preflight() {
  if [[ "${SKIP_PRECHECK:-0}" == "1" ]]; then
    echo "[online-install] Skipping pre-download preflight checks." >&2
    return 0
  fi

  echo "[online-install] Running pre-download preflight checks..." >&2

  # 1. OS check (Must be first to avoid Linux-specific command/path failures on non-Linux OS)
  if [[ "$(uname)" != "Linux" ]]; then
    echo "[online-install] ERROR: Cube Sandbox only supports Linux." >&2
    exit 3
  fi

  # 2. Root check (install.sh and services require root anyway)
  if [[ "${EUID}" -ne 0 ]]; then
    echo "[online-install] ERROR: This script must run as root." >&2
    echo "[online-install] Please run with sudo or as root user." >&2
    exit 1
  fi

  # 3. Essential commands for downloading and extracting
  local cmd
  for cmd in tar awk curl wget; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
      if [[ "${cmd}" == "curl" || "${cmd}" == "wget" ]]; then
        # We need at least one of curl or wget to download the bundle
        if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
          echo "[online-install] ERROR: required command not found: curl or wget" >&2
          exit 2
        fi
      else
        echo "[online-install] ERROR: required command not found: ${cmd}" >&2
        exit 2
      fi
    fi
  done

  # 4. python3 check (needed for JSON release discovery if --url is omitted)
  if [[ -z "${DOWNLOAD_URL}" ]] && ! command -v python3 >/dev/null 2>&1; then
    echo "[online-install] ERROR: required command not found: python3 (needed to parse latest release JSON)" >&2
    echo "[online-install] Please install python3 or specify the download URL manually with --url=<url>" >&2
    exit 2
  fi

  # 5. KVM check
  if [[ ! -e /dev/kvm ]]; then
    if [[ "${CUBE_PVM_ENABLE:-0}" == "1" ]]; then
      echo "[online-install] ERROR: PVM mode is enabled (CUBE_PVM_ENABLE=1), but /dev/kvm was not found." >&2
      echo "[online-install] Please make sure:" >&2
      echo "[online-install]   1. You have installed the PVM host kernel and rebooted into it (check with: uname -r)." >&2
      echo "[online-install]   2. The PVM KVM module is loaded on the host: sudo modprobe kvm_pvm" >&2
      echo "[online-install] For the full setup flow, see: https://cubesandbox.com/guide/pvm-deploy.html" >&2
      echo "[online-install] Note: Cube Sandbox requires /dev/kvm to function." >&2
    else
      echo "[online-install] ERROR: KVM is not supported or not enabled (/dev/kvm not found)." >&2
      echo "[online-install] If this host cannot expose hardware KVM (e.g., nested virtualization is disabled)," >&2
      echo "[online-install] you can use the prebuilt PVM kernel to enable KVM on this host first:" >&2
      echo "[online-install]   1. Download the prebuilt PVM kernel (RPM/DEB) matching your OS from Releases:" >&2
      echo "[online-install]        https://github.com/TencentCloud/CubeSandbox/releases" >&2
      echo "[online-install]   2. Follow the PVM deployment guide for host setup and installation steps:" >&2
      echo "[online-install]        https://cubesandbox.com/guide/pvm-deploy.html" >&2
      echo "[online-install] Note: Cube Sandbox requires /dev/kvm to function." >&2
    fi
    exit 3
  fi

  # 6. Memory check (>= 8GB or configurable threshold)
  local mem_total_kb
  mem_total_kb="$(awk '/MemTotal/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)"
  
  local min_mem_kb=7500000
  if [[ -n "${CUBE_MIN_MEMORY_KB:-}" ]]; then
    if [[ "${CUBE_MIN_MEMORY_KB}" =~ ^[0-9]+$ ]] && [[ "${CUBE_MIN_MEMORY_KB}" -gt 0 ]]; then
      # Enforce that the threshold cannot be lower than the default 8GB (7500000 KB)
      if [[ "${CUBE_MIN_MEMORY_KB}" -ge 7500000 ]]; then
        min_mem_kb="${CUBE_MIN_MEMORY_KB}"
      fi
    else
      echo "[online-install] ERROR: Invalid CUBE_MIN_MEMORY_KB '${CUBE_MIN_MEMORY_KB}' (must be a positive integer greater than 0)." >&2
      exit 2
    fi
  fi

  if [[ "${mem_total_kb}" -lt "${min_mem_kb}" ]]; then
    echo "[online-install] ERROR: System memory must be at least $((min_mem_kb / 1024 / 1024))GB (found $((mem_total_kb / 1024 / 1024)) GB)." >&2
    exit 3
  fi

  # 7. /data/cubelet XFS filesystem check
  local cubelet_dir="/data/cubelet"
  local check_path="${cubelet_dir}"
  while [[ ! -e "${check_path}" ]]; do
    local parent
    parent="$(dirname "${check_path}")"
    [[ "${parent}" != "${check_path}" ]] || break
    check_path="${parent}"
  done

  # Check if the closest existing parent directory is writable by root (detects read-only mounts)
  if [[ ! -w "${check_path}" ]]; then
    echo "[online-install] ERROR: Path '${check_path}' is not writable. It may be mounted on a read-only filesystem." >&2
    exit 1
  fi

  local fs_type
  fs_type="$(df -T "${check_path}" 2>/dev/null | awk 'NR==2 {print $2}')"
  if [[ "${fs_type}" != "xfs" ]]; then
    echo "[online-install] ERROR: The filesystem that will host /data/cubelet is on '${check_path}' (type: ${fs_type:-unknown}), which is not XFS." >&2
    echo "[online-install] Cube Sandbox requires the /data/cubelet directory to reside on an XFS filesystem." >&2
    echo "[online-install] Options:" >&2
    echo "[online-install]   1. Mount a dedicated XFS-formatted partition at /data/cubelet:" >&2
    echo "[online-install]        mkfs.xfs /dev/<your-partition>" >&2
    echo "[online-install]        mount /dev/<your-partition> /data/cubelet" >&2
    echo "[online-install]   2. Ensure the parent path (${check_path}) itself is on XFS." >&2
    exit 3
  fi

  # 8. Check deployment role early and check Docker/DNS installability (for control role)
  local deploy_role="${ONE_CLICK_DEPLOY_ROLE:-control}"
  case "${deploy_role}" in
    control|compute) ;;
    *)
      echo "[online-install] ERROR: Invalid ONE_CLICK_DEPLOY_ROLE '${deploy_role}' (expected 'control' or 'compute')." >&2
      exit 1
      ;;
  esac

  if [[ "${deploy_role}" != "compute" ]]; then
    # Verify package manager is available to install Docker if it is not present
    if ! command -v docker >/dev/null 2>&1; then
      if ! command -v apt-get >/dev/null 2>&1 && ! command -v yum >/dev/null 2>&1; then
        echo "[online-install] ERROR: Docker is not installed and no supported package manager (apt-get or yum) was found to install it." >&2
        exit 2
      fi
    fi

    # DNS check (requires resolvectl or NetworkManager loaded status)
    if ! command -v resolvectl >/dev/null 2>&1; then
      if command -v systemctl >/dev/null 2>&1; then
        local nm_load_state
        nm_load_state="$(systemctl show -p LoadState --value NetworkManager 2>/dev/null || true)"
        if [[ "${nm_load_state}" != "loaded" ]]; then
          echo "[online-install] ERROR: DNS setup requires resolvectl or NetworkManager." >&2
          exit 3
        fi
      else
        echo "[online-install] ERROR: DNS setup requires resolvectl or systemd/NetworkManager." >&2
        exit 3
      fi

      # Mirror check_dns_preflight: when resolvectl is absent and we fallback to NetworkManager,
      # we require either dnsmasq to be already installed, or a supported package manager to install it.
      if ! command -v dnsmasq >/dev/null 2>&1; then
        if ! command -v dnf >/dev/null 2>&1 && ! command -v yum >/dev/null 2>&1 && ! command -v apt-get >/dev/null 2>&1; then
          echo "[online-install] ERROR: resolvectl is absent and NetworkManager fallback is used, but dnsmasq is not installed and no supported package manager (dnf, yum, or apt-get) was found to install it." >&2
          exit 2
        fi
      fi
    fi
  fi

  echo "[online-install] Pre-download preflight checks passed." >&2
}

# Run early preflight checks before fetching release info or downloading large bundle
check_early_preflight

# ---------------------------------------------------------------------------
# Helper: HTTP GET to stdout (curl or wget)
# ---------------------------------------------------------------------------
http_get() {
  local url="$1"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "${url}"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO- "${url}"
  else
    echo "[online-install] ERROR: curl or wget is required" >&2
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# Auto-detect download URL if --url / CUBE_SANDBOX_DOWNLOAD_URL was not given.
#
# Discovery order:
#   1. MIRROR=cn   -> https://download.cubesandbox.com/release/latest.json
#                     (JSON body: {"url": "https://.../cube-sandbox-one-click-<sha>.tar.gz"})
#   2. default     -> GitHub API latest release asset
# ---------------------------------------------------------------------------
if [[ -z "${DOWNLOAD_URL}" ]]; then
  if [[ "${MIRROR}" == "cn" ]]; then
    echo "[online-install] MIRROR=cn, fetching latest release info from ${CN_MIRROR_LATEST_URL}..." >&2

    LATEST_JSON="$(http_get "${CN_MIRROR_LATEST_URL}")" || {
      echo "[online-install] ERROR: failed to fetch ${CN_MIRROR_LATEST_URL}." >&2
      echo "[online-install] You can specify the URL manually:" >&2
      echo "[online-install]   online-install.sh --url=<download-url> [install.sh options...]" >&2
      exit 1
    }

    DOWNLOAD_URL="$(python3 - "${LATEST_JSON}" <<'PY'
import json, sys

data = json.loads(sys.argv[1])
url = data.get("url", "")
if not url:
    sys.exit(1)
print(url)
PY
    )" || {
      echo "[online-install] ERROR: could not parse 'url' from ${CN_MIRROR_LATEST_URL}." >&2
      echo "[online-install] You can specify the URL manually:" >&2
      echo "[online-install]   online-install.sh --url=<download-url> [install.sh options...]" >&2
      exit 1
    }

    echo "[online-install] CN mirror latest asset: ${DOWNLOAD_URL}" >&2
  else
    echo "[online-install] no --url provided, fetching latest release from github.com/${GITHUB_REPO}..." >&2

    RELEASE_JSON="$(http_get "${GITHUB_API_BASE}/releases/latest")"

    # Extract the first browser_download_url that matches our tarball pattern.
    # We use Python (already required by the build scripts) for reliable JSON
    # parsing without needing jq.
    DOWNLOAD_URL="$(python3 - "${RELEASE_JSON}" <<'PY'
import json, sys, re

data = json.loads(sys.argv[1])
pattern = re.compile(r'^cube-sandbox-one-click-[0-9a-f]+\.tar\.gz$')
for asset in data.get("assets", []):
    if pattern.match(asset.get("name", "")):
        print(asset["browser_download_url"])
        sys.exit(0)
sys.exit(1)
PY
    )" || {
      echo "[online-install] ERROR: could not find a cube-sandbox-one-click-<sha>.tar.gz asset in the latest release." >&2
      echo "[online-install] You can specify the URL manually:" >&2
      echo "[online-install]   online-install.sh --url=<download-url> [install.sh options...]" >&2
      exit 1
    }

    echo "[online-install] latest release asset: ${DOWNLOAD_URL}" >&2
  fi
fi

# ---------------------------------------------------------------------------
# Derive the expected directory name from the tarball filename.
# The tarball produced by build-release-bundle.sh is always named
#   cube-sandbox-one-click-<git-short-sha>.tar.gz
# and extracts to a single top-level directory with the same stem.
# ---------------------------------------------------------------------------
TARBALL_FILENAME="${DOWNLOAD_URL##*/}"   # basename of URL
BUNDLE_DIRNAME="${TARBALL_FILENAME%.tar.gz}"

if [[ "${BUNDLE_DIRNAME}" != cube-sandbox-one-click-* ]]; then
  echo "[online-install] ERROR: unexpected tarball filename '${TARBALL_FILENAME}'." >&2
  echo "[online-install] Expected: cube-sandbox-one-click-<sha>.tar.gz" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Download
# ---------------------------------------------------------------------------
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

echo "[online-install] downloading ${TARBALL_FILENAME}..." >&2
if command -v curl >/dev/null 2>&1; then
  curl -fSL "${DOWNLOAD_URL}" -o "${WORK_DIR}/bundle.tar.gz"
elif command -v wget >/dev/null 2>&1; then
  wget -q "${DOWNLOAD_URL}" -O "${WORK_DIR}/bundle.tar.gz"
else
  echo "[online-install] ERROR: curl or wget is required" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Extract and verify
# ---------------------------------------------------------------------------
echo "[online-install] extracting ${TARBALL_FILENAME}..." >&2
tar -xzf "${WORK_DIR}/bundle.tar.gz" -C "${WORK_DIR}"

BUNDLE_DIR="${WORK_DIR}/${BUNDLE_DIRNAME}"
if [[ ! -d "${BUNDLE_DIR}" ]]; then
  echo "[online-install] ERROR: expected directory '${BUNDLE_DIRNAME}' not found after extraction." >&2
  echo "[online-install] The archive may be corrupted or have an unexpected layout." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Run install.sh
# ---------------------------------------------------------------------------
echo "[online-install] running install.sh (version ${BUNDLE_DIRNAME#cube-sandbox-one-click-})..." >&2
chmod +x "${BUNDLE_DIR}/install.sh"
"${BUNDLE_DIR}/install.sh" "${INSTALL_ARGS[@]+"${INSTALL_ARGS[@]}"}"

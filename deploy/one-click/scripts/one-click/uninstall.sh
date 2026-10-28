#!/usr/bin/env bash
# Remove a CubeSandbox one-click installation from the host while deliberately
# preserving /usr/local/services/cubetoolbox.  Keeping the toolbox makes the
# uninstall script and the installed release available for inspection until the
# operator explicitly removes that final directory.
set -uo pipefail

TOOLBOX_ROOT="/usr/local/services/cubetoolbox"
SYSTEMD_UNIT_DIR="${ONE_CLICK_SYSTEMD_UNIT_INSTALL_DIR:-/etc/systemd/system}"
RUNTIME_DIR="${ONE_CLICK_RUNTIME_DIR:-/var/run/cube-sandbox-one-click}"
LOG_DIR="${ONE_CLICK_LOG_DIR:-/var/log/cube-sandbox-one-click}"

RESET='' BOLD='' RED='' GREEN='' YELLOW='' CYAN=''
PRESERVED_DOCKER_IMAGE_IDS=()
PRESERVED_PODMAN_IMAGE_IDS=()
if [[ -t 2 && "${TERM:-dumb}" != "dumb" && -z "${NO_COLOR:-}" ]]; then
  RESET=$'\033[0m'
  BOLD=$'\033[1m'
  RED=$'\033[31m'
  GREEN=$'\033[32m'
  YELLOW=$'\033[33m'
  CYAN=$'\033[36m'
fi

log()  { printf '%s[one-click-uninstall]%s %s\n' "${CYAN}${BOLD}" "${RESET}" "$*" >&2; }
warn() { printf '%s[one-click-uninstall] WARNING:%s %s\n' "${YELLOW}${BOLD}" "${RESET}" "$*" >&2; }
die()  { printf '%s[one-click-uninstall] ERROR:%s %s\n' "${RED}${BOLD}" "${RESET}" "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Usage:
  uninstall.sh          confirm interactively, then remove everything except the toolbox and container images
  uninstall.sh check    report CubeSandbox leftovers without changing the host

The directory /usr/local/services/cubetoolbox and Docker/Podman images are
always preserved. After a successful uninstall/check, remove the toolbox
manually when it is no longer needed:

  rm -rf /usr/local/services/cubetoolbox
EOF
}

require_root() {
  [[ "$(id -u)" -eq 0 ]] || die "this command must run as root"
}

confirm_uninstall() {
  printf '\n%s%sCubeSandbox one-click uninstall%s\n' "${BOLD}" "${RED}" "${RESET}" >&2
  cat >&2 <<EOF

The following CubeSandbox-owned resources will be stopped and deleted:
  - systemd services, timers, targets, and running processes
  - Docker/Podman containers, volumes, and networks
  - host DNS configuration, virtual interfaces, iptables, and policy routes
  - configuration under /etc/cube*, runtime state, and installed command links
  - data under /data/cube*, /data/CubeMaster, and /data/log/Cube*
  - CubeSandbox logs and state under /var/log, /var/lib, and /run
EOF

  printf '\n%sPreserved:%s\n' "${BOLD}" "${RESET}" >&2
  printf '  - Docker/Podman images (no image will be removed)\n' >&2
  printf '  - %s\n' "${TOOLBOX_ROOT}\n\n" >&2

  [[ -t 0 ]] || die "interactive confirmation is required; run this command from a terminal"

  local answer
  printf '%sType y or yes to continue%s, or anything else to cancel: ' \
    "${BOLD}" "${RESET}" >&2
  IFS= read -r answer || die "confirmation input failed; nothing was changed"
  case "${answer,,}" in
    y|yes) ;;
    *)
      log "cancelled; nothing was changed"
      exit 0
      ;;
  esac
}

validate_cleanup_path() {
  local name="$1" path="$2"
  [[ "${path}" == /* ]] || die "${name} must be an absolute path: ${path}"
  [[ "${path}" != "/" && "${path}" != "${TOOLBOX_ROOT}" ]] \
    || die "refusing unsafe ${name}: ${path}"
}

systemd_available() {
  command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]
}

cube_unit_names() {
  local path name
  shopt -s nullglob
  for path in \
    "${SYSTEMD_UNIT_DIR}"/cube-sandbox-*.service \
    "${SYSTEMD_UNIT_DIR}"/cube-sandbox-*.timer \
    "${SYSTEMD_UNIT_DIR}"/cube-sandbox-*.target; do
    name="${path##*/}"
    printf '%s\n' "${name}"
  done
  shopt -u nullglob
}

stop_and_remove_units() {
  local -a units=()
  mapfile -t units < <(cube_unit_names | sort -u)

  if systemd_available && [[ "${#units[@]}" -gt 0 ]]; then
    systemctl stop "${units[@]}" >/dev/null 2>&1 || true
    systemctl disable "${units[@]}" >/dev/null 2>&1 || true
    systemctl reset-failed "${units[@]}" >/dev/null 2>&1 || true
  fi

  rm -rf \
    "${SYSTEMD_UNIT_DIR}"/cube-sandbox-*.service \
    "${SYSTEMD_UNIT_DIR}"/cube-sandbox-*.service.d \
    "${SYSTEMD_UNIT_DIR}"/cube-sandbox-*.timer \
    "${SYSTEMD_UNIT_DIR}"/cube-sandbox-*.target \
    "${SYSTEMD_UNIT_DIR}"/cube-sandbox-*.target.wants \
    "${SYSTEMD_UNIT_DIR}"/multi-user.target.wants/cube-sandbox-*.target

  if systemd_available; then
    systemctl daemon-reload
    systemctl reset-failed >/dev/null 2>&1 || true
  fi
}

stop_remaining_processes() {
  local pattern
  pattern='(^|/)(cubelet|cubemaster|cubeops|cube-api|cube-templatecenter|cube-runtime|containerd-shim-cube-rs|cube-egress)([[:space:]]|$)'
  command -v pkill >/dev/null 2>&1 || return 0
  pkill -TERM -f "${pattern}" 2>/dev/null || true
  sleep 1
  pkill -KILL -f "${pattern}" 2>/dev/null || true
}

run_installed_rollback_helpers() {
  local helper
  for helper in \
    "${TOOLBOX_ROOT}/scripts/one-click/down-cube-egress.sh" \
    "${TOOLBOX_ROOT}/scripts/one-click/down-dns.sh"; do
    if [[ -f "${helper}" ]]; then
      bash "${helper}" >/dev/null 2>&1 || warn "rollback helper failed; continuing with fallback cleanup: ${helper}"
    fi
  done
}

remove_docker_objects() {
  command -v docker >/dev/null 2>&1 || return 0

  local id item
  local -a ids=() networks=() volumes=()
  while IFS= read -r id; do
    [[ -n "${id}" ]] && ids+=("${id}")
  done < <(docker ps -a --format '{{.ID}}|{{.Names}}|{{.Image}}' 2>/dev/null \
    | awk 'tolower($0) ~ /cube|sandbox-code|juicedata\/mount/ {split($0, a, "|"); print a[1]}')

  for id in "${ids[@]}"; do
    item="$(docker inspect --format '{{.Image}}' "${id}" 2>/dev/null || true)"
    [[ -n "${item}" ]] && PRESERVED_DOCKER_IMAGE_IDS+=("${item}")
    while IFS= read -r item; do
      [[ -n "${item}" ]] && networks+=("${item}")
    done < <(docker inspect --format '{{range $name, $_ := .NetworkSettings.Networks}}{{$name}}{{"\n"}}{{end}}' "${id}" 2>/dev/null || true)
    while IFS= read -r item; do
      [[ -n "${item}" ]] && volumes+=("${item}")
    done < <(docker inspect --format '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}}{{"\n"}}{{end}}{{end}}' "${id}" 2>/dev/null || true)
  done

  [[ "${#ids[@]}" -eq 0 ]] || docker rm -f "${ids[@]}" >/dev/null 2>&1 || true
  for item in "${volumes[@]}"; do docker volume rm -f "${item}" >/dev/null 2>&1 || true; done
  while IFS= read -r item; do
    [[ -n "${item}" ]] && docker volume rm -f "${item}" >/dev/null 2>&1 || true
  done < <(docker volume ls --format '{{.Name}}' 2>/dev/null | awk 'tolower($0) ~ /cube|sandbox-code/')
  for item in "${networks[@]}"; do
    case "${item}" in bridge|host|none|'') ;; *) docker network rm "${item}" >/dev/null 2>&1 || true ;; esac
  done
  while IFS= read -r item; do
    [[ -n "${item}" ]] && docker network rm "${item}" >/dev/null 2>&1 || true
  done < <(docker network ls --format '{{.Name}}' 2>/dev/null | awk 'tolower($0) ~ /cube|sandbox-code/')
  while IFS= read -r item; do
    [[ -n "${item}" ]] && PRESERVED_DOCKER_IMAGE_IDS+=("${item}")
  done < <(docker images --format '{{.Repository}}:{{.Tag}}|{{.ID}}' 2>/dev/null \
    | awk 'tolower($1) ~ /cube|sandbox-code|juicedata\/mount/ {split($0, a, "|"); print a[2]}')
}

remove_podman_objects() {
  command -v podman >/dev/null 2>&1 || return 0
  local -a ids=()
  local id item
  mapfile -t ids < <(podman ps -a --format '{{.ID}}|{{.Names}}|{{.Image}}' 2>/dev/null \
    | awk 'tolower($0) ~ /cube|sandbox-code|juicedata\/mount/ {split($0, a, "|"); print a[1]}')
  for id in "${ids[@]}"; do
    item="$(podman inspect --format '{{.Image}}' "${id}" 2>/dev/null || true)"
    [[ -n "${item}" ]] && PRESERVED_PODMAN_IMAGE_IDS+=("${item}")
  done
  [[ "${#ids[@]}" -eq 0 ]] || podman rm -f "${ids[@]}" >/dev/null 2>&1 || true
  while IFS= read -r id; do
    [[ -n "${id}" ]] && podman volume rm -f "${id}" >/dev/null 2>&1 || true
  done < <(podman volume ls --format '{{.Name}}' 2>/dev/null | awk 'tolower($0) ~ /cube|sandbox-code/')
  while IFS= read -r id; do
    [[ -n "${id}" ]] && podman network rm -f "${id}" >/dev/null 2>&1 || true
  done < <(podman network ls --format '{{.Name}}' 2>/dev/null | awk 'tolower($0) ~ /cube|sandbox-code/')
  while IFS= read -r item; do
    [[ -n "${item}" ]] && PRESERVED_PODMAN_IMAGE_IDS+=("${item}")
  done < <(podman images --format '{{.Repository}}:{{.Tag}}|{{.ID}}' 2>/dev/null \
    | awk 'tolower($1) ~ /cube|sandbox-code|juicedata\/mount/ {split($0, a, "|"); print a[2]}')
}

print_image_cleanup_hint() {
  local image_id
  local -A seen=()
  printf '\n%sContainer images were preserved.%s\n' "${GREEN}${BOLD}" "${RESET}" >&2
  if command -v docker >/dev/null 2>&1; then
    if [[ "${#PRESERVED_DOCKER_IMAGE_IDS[@]}" -gt 0 ]]; then
      printf 'To remove the preserved Docker images from this uninstall:\n  docker image rm -f' >&2
      for image_id in "${PRESERVED_DOCKER_IMAGE_IDS[@]}"; do
        [[ -n "${image_id}" && -z "${seen[${image_id}]:-}" ]] || continue
        seen["${image_id}"]=1
        printf ' %q' "${image_id}" >&2
      done
      printf '\n' >&2
    else
      cat >&2 <<'EOF'
To remove currently identifiable CubeSandbox-related Docker images manually:
  docker images --format '{{.Repository}}:{{.Tag}} {{.ID}}' \
    | awk 'tolower($1) ~ /cube|sandbox-code|juicedata\/mount/ {print $2}' \
    | sort -u | xargs -r docker image rm -f
EOF
    fi
  fi
  if command -v podman >/dev/null 2>&1; then
    seen=()
    if [[ "${#PRESERVED_PODMAN_IMAGE_IDS[@]}" -gt 0 ]]; then
      printf 'To remove the preserved Podman images from this uninstall:\n  podman image rm -f' >&2
      for image_id in "${PRESERVED_PODMAN_IMAGE_IDS[@]}"; do
        [[ -n "${image_id}" && -z "${seen[${image_id}]:-}" ]] || continue
        seen["${image_id}"]=1
        printf ' %q' "${image_id}" >&2
      done
      printf '\n' >&2
    else
      cat >&2 <<'EOF'
To remove currently identifiable CubeSandbox-related Podman images manually:
  podman images --format '{{.Repository}}:{{.Tag}} {{.ID}}' \
    | awk 'tolower($1) ~ /cube|sandbox-code|juicedata\/mount/ {print $2}' \
    | sort -u | xargs -r podman image rm -f
EOF
    fi
  fi
}

remove_egress_network_rules() {
  if command -v iptables >/dev/null 2>&1; then
    while iptables -t mangle -C PREROUTING -j TRANSPROXY 2>/dev/null; do
      iptables -t mangle -D PREROUTING -j TRANSPROXY || break
    done
    iptables -t mangle -F TRANSPROXY 2>/dev/null || true
    iptables -t mangle -X TRANSPROXY 2>/dev/null || true
    iptables -t mangle -F TRANSPROXY.new 2>/dev/null || true
    iptables -t mangle -X TRANSPROXY.new 2>/dev/null || true
  fi

  command -v ip >/dev/null 2>&1 || return 0
  local mark port
  for mark in 0xce010000 0xce020000; do
    while ip rule del fwmark "${mark}/0xffff0000" table 100 2>/dev/null; do :; done
  done
  for port in 80 443; do
    while ip rule del iif cube-dev ipproto tcp dport "${port}" table 100 2>/dev/null; do :; done
  done
  ip route flush table 100 2>/dev/null || true
}

remove_dns_and_interfaces() {
  rm -f \
    /etc/NetworkManager/conf.d/90-cubeproxy-dns.conf \
    /etc/NetworkManager/dnsmasq.d/90-cubeproxy-cube-app.conf \
    /run/NetworkManager/system-connections/cube-*.nmconnection

  if command -v nmcli >/dev/null 2>&1; then
    local connection
    while IFS= read -r connection; do
      [[ -n "${connection}" ]] && nmcli connection delete "${connection}" >/dev/null 2>&1 || true
    done < <(nmcli -t -f NAME connection show 2>/dev/null | awk 'tolower($0) ~ /^cube-/')
  fi

  if command -v resolvectl >/dev/null 2>&1; then
    resolvectl revert cube-dns0 >/dev/null 2>&1 || true
  fi
  if command -v ip >/dev/null 2>&1; then
    local iface
    while IFS= read -r iface; do
      [[ -n "${iface}" ]] && ip link delete "${iface}" >/dev/null 2>&1 || true
    done < <(ip -o link show 2>/dev/null | awk -F': ' '{name=$2; sub(/@.*/, "", name); if (name ~ /^z?[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/) print name}')
    ip link delete cube-dns0 >/dev/null 2>&1 || true
    ip link delete cube-router >/dev/null 2>&1 || true
    ip link delete cube-dev >/dev/null 2>&1 || true
  fi
}

unmount_cube_mounts() {
  command -v findmnt >/dev/null 2>&1 || return 0
  local target
  while IFS= read -r target; do
    [[ -n "${target}" ]] || continue
    # /data/cubelet itself may be an operator-provisioned disk.  Empty it in
    # remove_data(), but do not unmount or reformat that disk.
    [[ "${target}" == "/data/cubelet" ]] && continue
    while mountpoint -q "${target}" 2>/dev/null; do
      umount "${target}" 2>/dev/null || umount -l "${target}" 2>/dev/null || break
    done
  done < <(findmnt -rn -o TARGET,SOURCE,FSTYPE 2>/dev/null \
    | awk 'tolower($0) ~ /cube|juice/ {print length($1), $1}' | sort -rn | cut -d' ' -f2-)
}

clear_path_preserving_mountpoint() {
  local path="$1"
  [[ -e "${path}" ]] || return 0
  if mountpoint -q "${path}" 2>/dev/null; then
    find "${path}" -xdev -mindepth 1 -delete 2>/dev/null || warn "could not completely empty mounted path ${path}"
  else
    rm -rf -- "${path}"
  fi
}

remove_data() {
  local path
  for path in \
    /data/cubelet /data/cube-shim /data/cube-shared /data/cubeops /data/CubeMaster \
    /data/log/Cubelet /data/log/CubeShim /data/log/CubeVmm /data/log/CubeAPI \
    /data/log/CubeOps /data/log/CubeMaster /data/log/CubeTemplateCenter \
    /data/log/cube-egress /data/log/cube-proxy; do
    clear_path_preserving_mountpoint "${path}"
  done

  rm -rf \
    /etc/cube /etc/cubeegress /etc/cube-sandbox \
    /var/lib/cube-sandbox-logrotate /var/lib/cube-node-bootstrap /var/lib/cubeops \
    "${RUNTIME_DIR}" "${LOG_DIR}" \
    /run/cube-sandbox-one-click /run/cube-sandbox-systemd /run/cube-containers \
    /run/cube-pause-timings /run/cubelock.db /run/cube-let.pid \
    /var/log/cube-* /run/cube-* /var/run/cube-*

  rm -f /run/vc/vm/*/cube.sock

  rm -f \
    /usr/local/bin/containerd-shim-cube-rs /usr/local/bin/cube-runtime \
    /usr/local/bin/cubecli /usr/local/bin/cubevsmapdump \
    /usr/local/bin/cubemastercli /usr/local/bin/cubeopscli \
    /usr/local/bin/cube-jfs-setup.sh /usr/local/bin/cube-jfs-init.sh \
    /usr/local/bin/cube-jfs-mount.sh /usr/local/bin/cube-jfs-check.sh
}

check_failed=0
report_leftover() {
  printf '%sLEFTOVER%s %-12s %s\n' "${RED}${BOLD}" "${RESET}" "$1" "$2"
  check_failed=1
}

report_preserved_image() {
  printf '%sPRESERVED%s %-11s %s\n' "${GREEN}${BOLD}" "${RESET}" image "$1"
}

check_path_glob() {
  local category="$1"
  shift
  local pattern path
  for pattern in "$@"; do
    while IFS= read -r path; do
      [[ -n "${path}" ]] && report_leftover "${category}" "${path}"
    done < <(compgen -G "${pattern}" || true)
  done
}

check_installation() {
  check_failed=0
  local line

  check_path_glob systemd \
    "${SYSTEMD_UNIT_DIR}/cube-sandbox-*.service" \
    "${SYSTEMD_UNIT_DIR}/cube-sandbox-*.service.d" \
    "${SYSTEMD_UNIT_DIR}/cube-sandbox-*.timer" \
    "${SYSTEMD_UNIT_DIR}/cube-sandbox-*.target" \
    "${SYSTEMD_UNIT_DIR}/cube-sandbox-*.target.wants" \
    "${SYSTEMD_UNIT_DIR}/multi-user.target.wants/cube-sandbox-*.target"

  if command -v pgrep >/dev/null 2>&1; then
    while IFS= read -r line; do report_leftover process "${line}"; done \
      < <(pgrep -af '(^|/)(cubelet|cubemaster|cubeops|cube-api|cube-templatecenter|cube-runtime|containerd-shim-cube-rs|cube-egress)([[:space:]]|$)' 2>/dev/null || true)
  fi
  if command -v docker >/dev/null 2>&1; then
    while IFS= read -r line; do report_leftover container "${line}"; done \
      < <(docker ps -a --format '{{.Names}} {{.Image}}' 2>/dev/null | awk 'tolower($0) ~ /cube|sandbox-code|juicedata\/mount/')
    while IFS= read -r line; do report_preserved_image "${line}"; done \
      < <(docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | awk 'tolower($0) ~ /cube|sandbox-code|juicedata\/mount/')
    while IFS= read -r line; do report_leftover volume "${line}"; done \
      < <(docker volume ls --format '{{.Name}}' 2>/dev/null | awk 'tolower($0) ~ /cube|sandbox-code/')
    while IFS= read -r line; do report_leftover network "${line}"; done \
      < <(docker network ls --format '{{.Name}}' 2>/dev/null | awk 'tolower($0) ~ /cube|sandbox-code/')
  fi
  if command -v podman >/dev/null 2>&1; then
    while IFS= read -r line; do report_leftover container "${line}"; done \
      < <(podman ps -a --format '{{.Names}} {{.Image}}' 2>/dev/null | awk 'tolower($0) ~ /cube|sandbox-code|juicedata\/mount/')
    while IFS= read -r line; do report_preserved_image "${line}"; done \
      < <(podman images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | awk 'tolower($0) ~ /cube|sandbox-code|juicedata\/mount/')
    while IFS= read -r line; do report_leftover volume "${line}"; done \
      < <(podman volume ls --format '{{.Name}}' 2>/dev/null | awk 'tolower($0) ~ /cube|sandbox-code/')
    while IFS= read -r line; do report_leftover network "${line}"; done \
      < <(podman network ls --format '{{.Name}}' 2>/dev/null | awk 'tolower($0) ~ /cube|sandbox-code/')
  fi
  if command -v findmnt >/dev/null 2>&1; then
    while IFS= read -r line; do report_leftover mount "${line}"; done \
      < <(findmnt -rn -o TARGET,SOURCE,FSTYPE 2>/dev/null \
        | awk '$1 != "/data/cubelet" && tolower($0) ~ /cube|juice/')
  fi
  if command -v ip >/dev/null 2>&1; then
    while IFS= read -r line; do report_leftover netdev "${line}"; done \
      < <(ip -o link show 2>/dev/null | awk -F': ' '{name=$2; sub(/@.*/, "", name); if (name ~ /cube-dev|cube-router|cube-dns0/ || name ~ /^z?[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/) print $0}')
    while IFS= read -r line; do report_leftover ip-rule "${line}"; done \
      < <(ip rule show 2>/dev/null | awk 'tolower($0) ~ /ce010000|ce020000/ || ($0 ~ /iif cube-dev/ && $0 ~ /lookup 100/)')
  fi
  if command -v iptables-save >/dev/null 2>&1; then
    while IFS= read -r line; do report_leftover iptables "${line}"; done \
      < <(iptables-save 2>/dev/null | awk 'tolower($0) ~ /transproxy|cube-dev/')
  fi

  check_path_glob config /etc/cube /etc/cubeegress /etc/cube-sandbox \
    /etc/NetworkManager/conf.d/90-cubeproxy-dns.conf \
    /etc/NetworkManager/dnsmasq.d/90-cubeproxy-cube-app.conf
  check_path_glob runtime "${RUNTIME_DIR}" /run/cube-sandbox-one-click \
    /run/cube-sandbox-systemd /run/cube-containers /run/cube-pause-timings \
    /run/cubelock.db /run/cube-let.pid
  check_path_glob runtime '/run/vc/vm/*/cube.sock'
  if [[ -e /data/cubelet ]]; then
    if ! mountpoint -q /data/cubelet 2>/dev/null; then
      report_leftover data /data/cubelet
    elif find /data/cubelet -xdev -mindepth 1 -print -quit 2>/dev/null | grep -q .; then
      report_leftover data "/data/cubelet (mounted path is not empty)"
    fi
  fi
  check_path_glob data /data/cube-shim /data/cube-shared \
    /data/cubeops /data/CubeMaster /var/lib/cube-sandbox-logrotate \
    /var/lib/cube-node-bootstrap /var/lib/cubeops
  check_path_glob logs "${LOG_DIR}" /data/log/Cubelet /data/log/CubeShim \
    /data/log/CubeVmm /data/log/CubeAPI /data/log/CubeOps /data/log/CubeMaster \
    /data/log/CubeTemplateCenter /data/log/cube-egress /data/log/cube-proxy
  check_path_glob binary /usr/local/bin/containerd-shim-cube-rs \
    /usr/local/bin/cube-runtime /usr/local/bin/cubecli /usr/local/bin/cubevsmapdump \
    /usr/local/bin/cubemastercli /usr/local/bin/cubeopscli \
    /usr/local/bin/cube-jfs-setup.sh /usr/local/bin/cube-jfs-init.sh \
    /usr/local/bin/cube-jfs-mount.sh /usr/local/bin/cube-jfs-check.sh

  if [[ "${check_failed}" -ne 0 ]]; then
    log "check failed: CubeSandbox leftovers were found"
    return 1
  fi
  log "check passed: no unexpected CubeSandbox leftovers were found outside ${TOOLBOX_ROOT}"
  return 0
}

uninstall() {
  local result=0
  require_root
  validate_cleanup_path ONE_CLICK_RUNTIME_DIR "${RUNTIME_DIR}"
  validate_cleanup_path ONE_CLICK_LOG_DIR "${LOG_DIR}"
  confirm_uninstall
  log "[1/6] stopping systemd services and removing units"
  stop_and_remove_units
  log "[2/6] rolling back DNS/egress helpers and stopping remaining processes"
  run_installed_rollback_helpers
  stop_remaining_processes
  log "[3/6] removing containers, volumes, and networks (images are preserved)"
  remove_docker_objects
  remove_podman_objects
  log "[4/6] removing host network and DNS state"
  remove_egress_network_rules
  remove_dns_and_interfaces
  log "[5/6] unmounting CubeSandbox mounts and removing data"
  unmount_cube_mounts
  remove_data

  log "[6/6] checking for unexpected leftovers"
  if ! check_installation; then
    warn "uninstall completed with leftovers; inspect the LEFTOVER lines above"
    result=1
  else
    log "uninstall complete; ${TOOLBOX_ROOT} was intentionally preserved"
  fi
  print_image_cleanup_hint
  printf '\n%sRemove the preserved toolbox manually when ready:%s\n  rm -rf %s\n\n' \
    "${YELLOW}${BOLD}" "${RESET}" "${TOOLBOX_ROOT}"
  return "${result}"
}

case "${1:-uninstall}" in
  uninstall) [[ "$#" -eq 0 || "$#" -eq 1 ]] || { usage >&2; exit 2; }; uninstall ;;
  check)
    [[ "$#" -eq 1 ]] || { usage >&2; exit 2; }
    check_result=0
    check_installation || check_result=$?
    print_image_cleanup_hint
    exit "${check_result}"
    ;;
  -h|--help|help) usage ;;
  *) usage >&2; exit 2 ;;
esac

#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
load_build_env

PREBUILT_DIR="${SCRIPT_DIR}/.work/prebuilt"
HELPER_SCRIPT="${SCRIPT_DIR}/.work/build-prebuilt-in-builder.sh"
ENVD_WORK_PATH="${SCRIPT_DIR}/.work/envd"
BUILDER_IMAGE_REF="${BUILDER_IMAGE:-cube-sandbox-builder:ubuntu2004}"

CUBE_VERSION_FROM_ENV="${CUBE_VERSION:-}"
LATEST_RELEASE_TAG="$(git -C "${ROOT_DIR}" describe --tags --abbrev=0 --match 'v*' 2>/dev/null || true)"
: "${CUBE_VERSION:=${LATEST_RELEASE_TAG:-0.0.0-dev}}"
: "${CUBE_COMMIT:=$(git -C "${ROOT_DIR}" rev-parse HEAD 2>/dev/null || echo 'unknown')}"
# Derive the build time from the HEAD commit date (UTC) rather than wall-clock.
# CUBE_BUILD_TIME is embedded into every binary (Go via -ldflags -X, Rust via
# build.rs with `rerun-if-env-changed=CUBE_BUILD_TIME`). A wall-clock value
# changes on every run and invalidates all incremental caches -- most painfully
# it re-triggers CubeAPI's fat-LTO final link, the slowest step in the bundle.
# Anchoring it to the commit keeps the value byte-identical across repeated runs
# on the same HEAD, so a second build reuses the Go build cache and the Rust
# target/ dirs. Still overridable via the environment for release pipelines that
# need a literal build timestamp.
: "${CUBE_BUILD_TIME:=$(TZ=UTC0 git -C "${ROOT_DIR}" show -s --format=%cd --date=format-local:'%Y-%m-%dT%H:%M:%SZ' HEAD 2>/dev/null || date -u +'%Y-%m-%dT%H:%M:%SZ')}"
: "${ONE_CLICK_DIST_VERSION:=${CUBE_VERSION_FROM_ENV:-${LATEST_RELEASE_TAG:-$(latest_git_revision "${ROOT_DIR}")}}}"
export CUBE_VERSION CUBE_COMMIT CUBE_BUILD_TIME ONE_CLICK_DIST_VERSION

require_cmd docker
require_cmd make

rm -rf "${PREBUILT_DIR}"
mkdir -p "${PREBUILT_DIR}" "$(dirname "${HELPER_SCRIPT}")"
rm -f "${ENVD_WORK_PATH}"
if [[ -n "${ENVD_LOCAL_PATH:-}" ]]; then
  ensure_file "${ENVD_LOCAL_PATH}"
  install -m 0755 "${ENVD_LOCAL_PATH}" "${ENVD_WORK_PATH}"
  log "embedding envd into cubemastercli from ${ENVD_LOCAL_PATH}"
fi

cat > "${HELPER_SCRIPT}" <<'SCRIPT_EOF'
#!/usr/bin/env bash
set -euo pipefail

# Version values are resolved by the host script and passed into this helper.

go_version_ldflags() {
  local version_pkg="$1"
  printf -- "-s -w -X '%s.Version=%s' -X '%s.Commit=%s' -X '%s.BuildTime=%s'" \
    "${version_pkg}" "${CUBE_VERSION}" \
    "${version_pkg}" "${CUBE_COMMIT}" \
    "${version_pkg}" "${CUBE_BUILD_TIME}"
}

CUBEMASTER_VERSION_PKG="github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/version"
CUBELET_VERSION_PKG="github.com/tencentcloud/CubeSandbox/Cubelet/pkg/version"
CUBEOPS_VERSION_PKG="github.com/tencentcloud/CubeSandbox/CubeOps/internal/version"

CUBEMASTER_LDFLAGS="$(go_version_ldflags "${CUBEMASTER_VERSION_PKG}")"
CUBELET_LDFLAGS="$(go_version_ldflags "${CUBELET_VERSION_PKG}")"
CUBEOPS_LDFLAGS="$(go_version_ldflags "${CUBEOPS_VERSION_PKG}")"

PREBUILT_DIR="/workspace/deploy/one-click/.work/prebuilt"
mkdir -p "${PREBUILT_DIR}"
rm -f \
  "${PREBUILT_DIR}/cubemaster" \
  "${PREBUILT_DIR}/cubemastercli" \
  "${PREBUILT_DIR}/cubelet" \
  "${PREBUILT_DIR}/cubecli" \
  "${PREBUILT_DIR}/cube-api" \
  "${PREBUILT_DIR}/cubeops" \
  "${PREBUILT_DIR}/cubeopscli" \
  "${PREBUILT_DIR}/cubevsmapdump" \
  "${PREBUILT_DIR}/cube-agent" \
  "${PREBUILT_DIR}/cube-init" \
  "${PREBUILT_DIR}/containerd-shim-cube-rs" \
  "${PREBUILT_DIR}/cube-runtime"

# Component builds are split into independent tracks that run concurrently.
# Cross-step dependencies that must be serialized:
#   * cubecow static lib -> cubelet cgo link (inside track_cubelet)
#   * cubevs BPF `go generate` -> cubelet (embedded network runtime) +
#     cubevsmapdump; hoisted to the serial pre-launch step with Cubelet proto
#   * Cubelet proto generation -> BOTH track_cubelet AND track_cubemaster.
#     CubeMaster/go.mod has `replace .../Cubelet => ../Cubelet` and imports
#     Cubelet/api/services/volumeplugin/v1, so `go build ./cmd/cubemaster`
#     compiles Cubelet's generated *.pb.go. `make proto` rewrites those files
#     in place (truncate + rewrite), which on a cold Go cache can tear a
#     .pb.go while the cubemaster compiler is reading it. Because the consumer
#     spans two tracks, the gen step is hoisted to a single serial pre-launch
#     step below rather than living inside track_cubelet.
# Go and Cargo both lock their shared caches (GOPATH/GOCACHE under $HOME,
# CARGO_HOME), and each Rust project owns a separate target/, so parallel builds
# do not corrupt each other. Per-track output is captured to a log so concurrent
# stderr does not interleave; a failing track's log is dumped before the script
# exits.
LOG_DIR="/workspace/deploy/one-click/.work/build-logs"
rm -rf "${LOG_DIR}"
mkdir -p "${LOG_DIR}"

# Bounded parallelism. Running every track at once is fastest on a big host but
# can exhaust RAM on small CI runners: several tracks each kick off a fat-LTO
# cargo link (CubeAPI, CubeShim, cube-agent) plus Go compiles, and a 4c/8G box
# doing several concurrent LTO links at once will OOM. Auto-derive a safe cap
# from available CPUs and memory (whichever is smaller), budgeting ~3 GiB peak
# per heavy track -- a fat-LTO Rust final link can spike well above 2 GiB, so
# the more conservative divisor keeps memory-constrained hosts (8 GiB boxes,
# shared CI runners) off the OOM edge at the cost of a little parallelism there.
# Override with ONE_CLICK_BUILD_JOBS to force a value (e.g. 1 for a fully serial,
# maximally-stable build, or a large number to uncap on a big host).
detect_cpu_count() {
  if command -v nproc >/dev/null 2>&1; then
    nproc 2>/dev/null && return 0
  fi
  getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1
}

# Read the container's cgroup memory ceiling in bytes, or nothing when there is
# no finite limit. /proc/meminfo reports the *host* total, so on a cgroup-capped
# job container (e.g. a 4 GiB limit on a 64 GiB host) it would let the scheduler
# start more concurrent LTO links than the container can hold -- exactly the OOM
# this cap exists to prevent. cgroup v2 exposes memory.max ("max" == unlimited);
# v1 exposes memory.limit_in_bytes (a huge sentinel ~= unlimited). nproc already
# respects CPU quotas, so only memory needs this clamp.
detect_cgroup_mem_bytes() {
  local v
  if v="$(cat /sys/fs/cgroup/memory.max 2>/dev/null)"; then
    [[ "${v}" =~ ^[0-9]+$ ]] && { echo "${v}"; return 0; }
    return 0  # "max" or unreadable -> no finite limit
  fi
  v="$(cat /sys/fs/cgroup/memory/memory.limit_in_bytes 2>/dev/null)" || return 0
  # v1 uses a near-PAGE_COUNTER_MAX sentinel for "unlimited"; treat >= 8 EiB
  # (well above any real RAM, below the sentinel) as no finite limit.
  if [[ "${v}" =~ ^[0-9]+$ && "${v}" -lt 9223372036854771712 ]]; then
    echo "${v}"
  fi
}

detect_mem_gib() {
  # MemAvailable is the honest "usable without swapping" figure; fall back to
  # MemTotal, then to a conservative assumption when /proc is unreadable.
  local kb bytes cgroup_bytes
  kb="$(awk '/^MemAvailable:/{print $2; exit}' /proc/meminfo 2>/dev/null)"
  [[ -n "${kb}" ]] || kb="$(awk '/^MemTotal:/{print $2; exit}' /proc/meminfo 2>/dev/null)"
  if [[ -n "${kb}" && "${kb}" =~ ^[0-9]+$ ]]; then
    bytes=$(( kb * 1024 ))
  else
    echo 4
    return 0
  fi
  # Clamp to the cgroup limit when it is smaller than the host-reported figure.
  cgroup_bytes="$(detect_cgroup_mem_bytes)"
  if [[ -n "${cgroup_bytes}" && "${cgroup_bytes}" -gt 0 && "${cgroup_bytes}" -lt "${bytes}" ]]; then
    bytes="${cgroup_bytes}"
  fi
  echo $(( bytes / 1024 / 1024 / 1024 ))
}

resolve_build_jobs() {
  local total_tracks="$1"
  if [[ -n "${ONE_CLICK_BUILD_JOBS:-}" ]]; then
    if [[ "${ONE_CLICK_BUILD_JOBS}" =~ ^[0-9]+$ && "${ONE_CLICK_BUILD_JOBS}" -ge 1 ]]; then
      echo "${ONE_CLICK_BUILD_JOBS}"
      return 0
    fi
    echo "[one-click] WARNING: ignoring invalid ONE_CLICK_BUILD_JOBS='${ONE_CLICK_BUILD_JOBS}'" >&2
  fi
  local cpus mem_gib jobs_by_mem jobs
  cpus="$(detect_cpu_count)"
  [[ "${cpus}" =~ ^[0-9]+$ && "${cpus}" -ge 1 ]] || cpus=1
  mem_gib="$(detect_mem_gib)"
  # Budget ~3 GiB per concurrent heavy (LTO) track; always allow at least 1.
  jobs_by_mem=$(( mem_gib / 3 ))
  [[ "${jobs_by_mem}" -ge 1 ]] || jobs_by_mem=1
  jobs="${cpus}"
  [[ "${jobs_by_mem}" -lt "${jobs}" ]] && jobs="${jobs_by_mem}"
  [[ "${total_tracks}" -lt "${jobs}" ]] && jobs="${total_tracks}"
  [[ "${jobs}" -ge 1 ]] || jobs=1
  echo "${jobs}"
}

# Initialize as empty (=()) rather than a bare `declare -A`: under `set -u` an
# associative array that has never been assigned trips "unbound variable" when
# expanded as ${#arr[@]}, which the scheduler does before the first launch.
declare -A TRACK_PID=()
declare -A TRACK_LOG=()
declare -A TRACK_STATUS_FILE=()
# Names of tracks queued but not yet started (bounded scheduler input).
TRACK_QUEUE=()
# Registry of track name -> function so the scheduler can launch on demand.
declare -A TRACK_FUNC=()

queue_track() {
  local name="$1"
  local func="$2"
  TRACK_FUNC["${name}"]="${func}"
  TRACK_QUEUE+=("${name}")
}

_launch_track() {
  local name="$1"
  local log="${LOG_DIR}/${name}.log"
  local status_file="${LOG_DIR}/${name}.status"
  TRACK_LOG["${name}"]="${log}"
  TRACK_STATUS_FILE["${name}"]="${status_file}"
  rm -f "${status_file}"
  echo "[one-click] starting build track: ${name}" >&2
  # The subshell records its own exit status to a per-track file via an EXIT
  # trap. We read that file instead of re-`wait`ing the pid: once `wait -n`
  # reaps a child, a later `wait "${pid}"` on that same (now non-child) pid is
  # documented to return 127, and the exact behaviour varies across bash
  # versions -- fragile to rely on. The trap fires on *every* exit path,
  # including an explicit `exit N` inside a track and a `set -e` abort, so the
  # real status is captured (a plain trailing `echo "$?"` would be skipped on
  # those paths). It is version-independent and pins the status to the correct
  # track even when several finish in the same burst.
  #
  # `set -m` places the backgrounded subshell in its own process group with
  # PGID == PID (a plain `&` would inherit this script's group, since the script
  # is not a job-control shell when run under make/CI). That lets _reap_one
  # group-kill (`kill -- -PID`) a track's whole descendant tree. It matters on
  # the OOM path: if the subshell is SIGKILLed, its EXIT trap never runs and its
  # children (cargo/go/make) are orphaned and keep allocating memory until the
  # container exits, compounding the very memory pressure that triggered the
  # kill. Monitor mode is scoped to the launch only (set +m right after) so the
  # subsequent `wait -n` in _reap_one prints no async job-control chatter.
  set -m
  ( trap 'echo "$?" > "'"${status_file}"'"' EXIT; "${TRACK_FUNC[${name}]}" ) >"${log}" 2>&1 &
  TRACK_PID["${name}"]=$!
  set +m
}

# _reap_one: block until any running track exits, then dump its full log. The
# log is always shown, on success as well as failure: per-track redirection
# (see _launch_track) hides compiler warnings and other non-fatal diagnostics
# that exit 0, so replaying every track's output is the only way a caller sees
# them. Records failures into FAILED. Returns 0.
FAILED=0
_reap_one() {
  local name pid status
  # `wait -n` returns when the next child exits; we then identify which track.
  wait -n 2>/dev/null || true
  for name in "${!TRACK_PID[@]}"; do
    pid="${TRACK_PID[${name}]}"
    if ! kill -0 "${pid}" 2>/dev/null; then
      # Prefer the status the subshell recorded for itself. Fall back to a
      # nonzero sentinel only if the file is missing/garbage, which can only
      # happen if the subshell died before writing it (e.g. OOM-killed) -- in
      # which case treating the track as failed is exactly right.
      status="$(cat "${TRACK_STATUS_FILE[${name}]}" 2>/dev/null || true)"
      if [[ "${status}" =~ ^[0-9]+$ ]]; then
        if [[ "${status}" -eq 0 ]]; then
          echo "[one-click] build track OK: ${name}" >&2
        else
          FAILED=1
          echo "[one-click] ERROR: build track FAILED: ${name} (exit ${status})" >&2
        fi
      else
        # No status file -> the subshell died before its EXIT trap ran (e.g.
        # OOM-kill / SIGKILL). Treat as failed, and group-kill the track's
        # process group (see _launch_track's `set -m`) so its orphaned
        # cargo/go/make children are reaped instead of surviving and holding
        # memory. The leader is already gone; the negative-PID signal targets
        # the surviving group members. Harmless if the group is already empty.
        status=1
        FAILED=1
        kill -- -"${pid}" 2>/dev/null || true
        echo "[one-click] ERROR: build track FAILED: ${name} (killed before status recorded; reaped orphaned children)" >&2
      fi
      echo "[one-click] ----- begin ${name} log -----" >&2
      cat "${TRACK_LOG[${name}]}" >&2 || true
      echo "[one-click] ----- end ${name} log -----" >&2
      unset 'TRACK_PID['"${name}"']'
      return 0
    fi
  done
  return 0
}

run_tracks() {
  local max_jobs
  max_jobs="$(resolve_build_jobs "${#TRACK_QUEUE[@]}")"
  echo "[one-click] running ${#TRACK_QUEUE[@]} build tracks with up to ${max_jobs} concurrent (override via ONE_CLICK_BUILD_JOBS)" >&2

  local name
  for name in "${TRACK_QUEUE[@]}"; do
    while [[ "${#TRACK_PID[@]}" -ge "${max_jobs}" ]]; do
      _reap_one
    done
    _launch_track "${name}"
  done

  # Drain the rest.
  #
  # This is a deliberate full drain, not fail-fast: even after a track fails we
  # let the remaining tracks finish rather than killing the group. _reap_one
  # replays every track's full log on completion (see its comment), so draining
  # preserves the diagnostics from all components of a broken build in one run
  # -- often several independent failures surface together instead of one per
  # re-run. The cost is that a failed build still takes roughly the full
  # wall-clock; that is an accepted trade-off for local/CI debuggability. This
  # applies at every concurrency level, including ONE_CLICK_BUILD_JOBS=1: the
  # launch loop above never inspects FAILED, so a serial build also runs every
  # queued track and reports all failures at the end rather than stopping at the
  # first. run_tracks then returns nonzero if any track failed.
  while [[ "${#TRACK_PID[@]}" -gt 0 ]]; do
    _reap_one
  done

  return "${FAILED}"
}

track_cubemaster() {
  cd /workspace/CubeMaster
  go mod download
  # Consumes Cubelet's generated volumeplugin *.pb.go via the go.mod replace;
  # those sources are regenerated by the serial pre-launch proto step, never
  # concurrently with this build.
  go build -ldflags "${CUBEMASTER_LDFLAGS}" -o "${PREBUILT_DIR}/cubemaster" ./cmd/cubemaster
}

track_cubelet() {
  mkdir -p /workspace/_output/bin
  ( cd /workspace && IN_CUBE_SANDBOX_BUILDER=1 make cubecow-sdk )
  cd /workspace/Cubelet
  go mod download
  # NB: `make proto` is NOT run here -- it is hoisted to a serial pre-launch
  # step because track_cubemaster also consumes the generated Cubelet sources
  # (see the dependency notes above).
  go build -ldflags "${CUBELET_LDFLAGS}" -o /workspace/_output/bin/cubelet ./cmd/cubelet
  go build -ldflags "${CUBELET_LDFLAGS}" -o /workspace/_output/bin/cubecli ./cmd/cubecli
  install -m 0755 /workspace/_output/bin/cubelet "${PREBUILT_DIR}/cubelet"
  install -m 0755 /workspace/_output/bin/cubecli "${PREBUILT_DIR}/cubecli"
}

track_cube_api() {
  cd /workspace/CubeAPI
  cargo build --release --locked
  install -m 0755 /workspace/CubeAPI/target/release/cube-api "${PREBUILT_DIR}/cube-api"
}

track_cubeops() {
  cd /workspace/CubeOps
  go mod download
  go build -ldflags "${CUBEOPS_LDFLAGS}" -o "${PREBUILT_DIR}/cubeops" ./cmd/cubeops
}

track_netstack() {
  # cubevs BPF bindings are generated in the serial pre-launch step because
  # track_cubelet also links CubeNet/cubevs after the network runtime embed.
  ( cd /workspace/CubeNet/cubevs && go build -o "${PREBUILT_DIR}/cubevsmapdump" ./cmd/cubevsmapdump )
}

track_agent() {
  # Agent Makefile reads CUBE_VERSION/CUBE_COMMIT/CUBE_BUILD_TIME directly.
  ( cd /workspace/agent && make -j1 )
  make -C /workspace/agent BINDIR="${PREBUILT_DIR}" install
}

track_cube_init() {
  # guest-init builds cube-init, the Rust musl PID-1 binary that runs inside the
  # VM (decoupled from the guest image). Its own Cargo project + target/ dir, so
  # it is parallel-safe alongside the other Rust/Go tracks.
  ( cd /workspace/guest-init && make -j1 )
  make -C /workspace/guest-init BINDIR="${PREBUILT_DIR}" install
}

track_shim() {
  # CUBE_VERSION/COMMIT/BUILD_TIME picked up by shim/build.rs and cube-runtime/build.rs
  cd /workspace/CubeShim
  cargo build --release --locked
  install -m 0755 /workspace/CubeShim/target/release/containerd-shim-cube-rs "${PREBUILT_DIR}/containerd-shim-cube-rs"
  install -m 0755 /workspace/CubeShim/target/release/cube-runtime "${PREBUILT_DIR}/cube-runtime"
}

track_s3lvol() {
  # CubeS3lvol: NVMe/TCP target over S3/COS, statically linked against
  # SPDK/DPDK/AWS CRT. setup_dep.sh reuses /opt/s3lvol-* from the builder when
  # stamps match, else populates deps/.
  # --skip-smoke defers the DPDK EAL runtime check to deployment (the EAL needs
  # a real CPU affinity mask the builder cannot guarantee); the static
  # verify_binary gate (readelf/ldd) still runs.
  local s3lvol_jobs="${ONE_CLICK_S3LVOL_JOBS:-$(nproc)}"
  local stage="${PREBUILT_DIR}/s3lvol-stage"
  ( cd /workspace/CubeS3lvol && AWS_BUILD_TYPE=RelWithDebInfo ./setup_dep.sh --jobs "${s3lvol_jobs}" ) >&2
  ( cd /workspace/CubeS3lvol && AWS_BUILD_TYPE=RelWithDebInfo make S3LVOL_BUILD_TYPE=release -j"${s3lvol_jobs}" ) >&2
  rm -rf "${stage}" "${PREBUILT_DIR}/s3lvol"
  ( cd /workspace/CubeS3lvol && AWS_BUILD_TYPE=RelWithDebInfo ./make_release.sh --no-tar --skip-smoke --version "${CUBE_VERSION}" --outdir "${stage}" ) >&2
  mv "${stage}"/s3lvol-* "${PREBUILT_DIR}/s3lvol"
  rm -rf "${stage}"
  [[ -x "${PREBUILT_DIR}/s3lvol/bin/s3lvol_tgt" ]] || {
    echo "[one-click] CubeS3lvol release missing bin/s3lvol_tgt" >&2
    exit 1
  }
}

# Serial pre-launch step: build cubemastercli with the client-supplied envd
# payload when present. The envd payload must be embedded into the
# cubemastercli binary before packaging, but that build cannot safely share the
# later track_cubemaster path because the latter would overwrite the artifact.
echo "[one-click] building cubemastercli in builder" >&2
if [[ -f /workspace/deploy/one-click/.work/envd ]]; then
  (cd /workspace/CubeMaster && make cubemastercli ENVD_LOCAL_PATH="/workspace/deploy/one-click/.work/envd")
else
  (cd /workspace/CubeMaster && make cubemastercli)
fi
install -m 0755 /workspace/CubeMaster/build/cubemastercli "${PREBUILT_DIR}/cubemastercli"

# Serial pre-launch steps: generate Cubelet protobuf sources and cubevs BPF
# bindings exactly once before any track starts. Both track_cubelet and
# track_cubemaster compile the generated *.pb.go; track_cubelet and
# track_netstack both need the cubevs bpf2go outputs (see dependency notes).

echo "[one-click] generating Cubelet proto sources (serial, pre-tracks)" >&2
( cd /workspace/Cubelet && make proto ) >&2
echo "[one-click] generating CubeNet/cubevs bpf2go outputs (serial, pre-tracks)" >&2
( cd /workspace/CubeNet/cubevs && make gen ) >&2

echo "[one-click] building one-click components in parallel tracks" >&2
# Queue order matters under a low concurrency cap: front-load the slowest,
# LTO-heavy tracks (cube-api fat-LTO, shim, cube-agent) so they start first and
# overlap the lighter Go tracks instead of tailing the build.
queue_track s3lvol     track_s3lvol
queue_track cube-api   track_cube_api
queue_track shim       track_shim
queue_track agent      track_agent
queue_track cube-init  track_cube_init
queue_track cubelet    track_cubelet
queue_track cubemaster track_cubemaster
queue_track netstack   track_netstack
queue_track cubeops    track_cubeops

echo "[one-click] building cubeopscli in builder" >&2
(cd /workspace/CubeOps && go build -ldflags "-s -w" -o "${PREBUILT_DIR}/cubeopscli" ./cmd/cubeopscli)

run_tracks
SCRIPT_EOF

chmod 0755 "${HELPER_SCRIPT}"

if ! docker image inspect "${BUILDER_IMAGE_REF}" >/dev/null 2>&1; then
  log "builder image ${BUILDER_IMAGE_REF} missing, building it first"
  make -C "${ROOT_DIR}" builder-image BUILDER_IMAGE="${BUILDER_IMAGE_REF}" >&2
fi

log "building one-click component binaries in builder"
make -C "${ROOT_DIR}" builder-run \
  BUILDER_IMAGE="${BUILDER_IMAGE_REF}" \
  BUILDER_CMD="bash /workspace/deploy/one-click/.work/build-prebuilt-in-builder.sh" >&2

for artifact in \
  cubemaster \
  cubemastercli \
  cubelet \
  cubecli \
  cube-api \
  cubeops \
  cubeopscli \
  cubevsmapdump \
  cube-agent \
  cube-init \
  containerd-shim-cube-rs \
  cube-runtime
do
  ensure_file "${PREBUILT_DIR}/${artifact}"
done

# CubeS3lvol produces a release *directory* (bin/ + scripts/ + VERSION)
# rather than a single binary, so verify it separately.
ensure_dir "${PREBUILT_DIR}/s3lvol"
ensure_file "${PREBUILT_DIR}/s3lvol/bin/s3lvol_tgt"

log "packaging one-click release bundle on host with prebuilt artifacts"
ONE_CLICK_CUBEMASTER_BIN="${PREBUILT_DIR}/cubemaster" \
ONE_CLICK_CUBEMASTERCLI_BIN="${PREBUILT_DIR}/cubemastercli" \
ONE_CLICK_CUBELET_BIN="${PREBUILT_DIR}/cubelet" \
ONE_CLICK_CUBECLI_BIN="${PREBUILT_DIR}/cubecli" \
ONE_CLICK_CUBE_API_BIN="${PREBUILT_DIR}/cube-api" \
ONE_CLICK_CUBE_OPS_BIN="${PREBUILT_DIR}/cubeops" \
ONE_CLICK_CUBE_OPS_CLI_BIN="${PREBUILT_DIR}/cubeopscli" \
ONE_CLICK_CUBEVSMAPDUMP_BIN="${PREBUILT_DIR}/cubevsmapdump" \
ONE_CLICK_CUBE_AGENT_BIN="${PREBUILT_DIR}/cube-agent" \
ONE_CLICK_CUBE_INIT_BIN="${PREBUILT_DIR}/cube-init" \
ONE_CLICK_CUBESHIM_BIN="${PREBUILT_DIR}/containerd-shim-cube-rs" \
ONE_CLICK_CUBE_RUNTIME_BIN="${PREBUILT_DIR}/cube-runtime" \
ONE_CLICK_S3LVOL_DIR="${PREBUILT_DIR}/s3lvol" \
  "${SCRIPT_DIR}/build-release-bundle.sh" "$@"

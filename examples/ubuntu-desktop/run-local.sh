#!/usr/bin/env bash
# Build and run the desktop image on Linux Docker for local debug.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "${ROOT}"

IMAGE="${IMAGE:-cube-ubuntu-desktop:local}"
NAME="${NAME:-cube-ubuntu-desktop}"
NOVNC_PORT="${NOVNC_PORT:-6080}"
ENVD_PORT="${ENVD_PORT:-49983}"
SHM_SIZE="${SHM_SIZE:-1g}"

if ! command -v docker >/dev/null 2>&1; then
    echo "docker is not installed or not on PATH" >&2
    exit 1
fi

echo "==> building ${IMAGE} (progress=plain; apt uses Tencent Ubuntu mirror by default)"
# ubuntu-session / GNOME is a large apt set. Without a CN mirror this
# step can sit on archive.ubuntu.com for 30–60 minutes with little output.
docker build --progress=plain -t "${IMAGE}" .

if docker ps -a --format '{{.Names}}' | grep -qx "${NAME}"; then
    echo "==> removing existing container ${NAME}"
    docker rm -f "${NAME}" >/dev/null
fi

echo "==> running ${NAME} (shm=${SHM_SIZE})"
# Dummy Xorg inside Docker sometimes needs SYS_TTY_CONFIG; shm is for X/Xfce.
docker run -d \
    --name "${NAME}" \
    --shm-size="${SHM_SIZE}" \
    --cap-add=SYS_TTY_CONFIG \
    -p "${NOVNC_PORT}:6080" \
    -p "${ENVD_PORT}:49983" \
    "${IMAGE}"

echo "==> waiting for noVNC on http://127.0.0.1:${NOVNC_PORT}/"
ok=0
for i in $(seq 1 60); do
    if curl -sf -o /dev/null "http://127.0.0.1:${NOVNC_PORT}/vnc.html"; then
        ok=1
        break
    fi
    if ! docker ps --format '{{.Names}}' | grep -qx "${NAME}"; then
        echo "container exited; last logs:" >&2
        docker logs "${NAME}" >&2 || true
        exit 1
    fi
    sleep 1
done

if [ "${ok}" -ne 1 ]; then
    echo "noVNC did not become ready; logs:" >&2
    docker logs "${NAME}" >&2 || true
    exit 1
fi

echo "==> checking DISPLAY inside the container"
docker exec "${NAME}" bash -lc 'xdpyinfo | awk "/dimensions:/{print}"'
docker exec "${NAME}" bash -lc 'python3 -c "from Xlib import display; d=display.Display(); print(\"Xlib\", d.screen().root.get_geometry().width, \"x\", d.screen().root.get_geometry().height)"'
echo "==> envd /health => $(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:${ENVD_PORT}/health")"

cat <<EOF

Desktop is up.

  noVNC:  http://127.0.0.1:${NOVNC_PORT}/
  envd:   http://127.0.0.1:${ENVD_PORT}/health

Logs:    docker logs -f ${NAME}
Shell:   docker exec -it ${NAME} bash
Stop:    docker rm -f ${NAME}

If the desktop is black, wait a few seconds for Xfce, then refresh noVNC.
EOF

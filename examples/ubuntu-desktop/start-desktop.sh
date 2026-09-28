#!/bin/bash
# Start dummy Xorg (or Xvfb fallback), Ubuntu GNOME, x11vnc, and noVNC.
# Foreground process is this script so cube-entrypoint / docker stop can signal it.
set -euo pipefail

DISPLAY_NUM="${DISPLAY:-:0}"
SCREEN_WIDTH="${SCREEN_WIDTH:-1280}"
SCREEN_HEIGHT="${SCREEN_HEIGHT:-720}"
SCREEN_DEPTH="${SCREEN_DEPTH:-24}"
VNC_PORT="${VNC_PORT:-5900}"
NOVNC_PORT="${NOVNC_PORT:-6080}"
DESKTOP_USER="${DESKTOP_USER:-user}"
NOVNC_WEB="${NOVNC_WEB:-/usr/share/novnc}"
XORG_LOG="${XORG_LOG:-/var/log/Xorg.0.log}"
FORCE_DUMMY="${FORCE_DUMMY:-0}"
DISPLAY_BACKEND=""

export DISPLAY="${DISPLAY_NUM}"
export HOME="${HOME:-/home/${DESKTOP_USER}}"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/tmp/runtime-${DESKTOP_USER}}"
export XORG_RUN_AS_USER_OK=1

mkdir -p /tmp/.X11-unix "${XDG_RUNTIME_DIR}" /var/log "/home/${DESKTOP_USER}"
chmod 1777 /tmp/.X11-unix
chown "${DESKTOP_USER}:${DESKTOP_USER}" "${XDG_RUNTIME_DIR}" "/home/${DESKTOP_USER}"
chmod 700 "${XDG_RUNTIME_DIR}"

rm -f /tmp/.X0-lock /tmp/.X11-unix/X0 /tmp/.X11-unix/X0.lock

PIDS=()

cleanup() {
    trap - TERM INT HUP EXIT
    for pid in "${PIDS[@]:-}"; do
        kill "${pid}" 2>/dev/null || true
    done
    wait 2>/dev/null || true
}
trap cleanup TERM INT HUP EXIT

log() {
    echo "start-desktop: $*" >&2
}

wait_for_display() {
    local tries="${1:-50}"
    local i
    for i in $(seq 1 "${tries}"); do
        if [ -S /tmp/.X11-unix/X0 ] && xdpyinfo -display "${DISPLAY_NUM}" >/dev/null 2>&1; then
            return 0
        fi
        sleep 0.2
    done
    return 1
}

start_dummy_xorg() {
    log "starting Xorg dummy on ${DISPLAY_NUM} (${SCREEN_WIDTH}x${SCREEN_HEIGHT})"
    Xorg "${DISPLAY_NUM}" \
        -config /etc/X11/xorg.conf \
        -noreset \
        -nolisten tcp \
        -novtswitch \
        +extension COMPOSITE \
        +extension RANDR \
        +extension RENDER \
        +extension GLX \
        -logfile "${XORG_LOG}" &
    PIDS+=("$!")
    if wait_for_display 50; then
        DISPLAY_BACKEND="dummy-xorg"
        return 0
    fi
    log "Xorg dummy did not become ready"
    if [ -f "${XORG_LOG}" ]; then
        log "----- ${XORG_LOG} (tail) -----"
        tail -n 80 "${XORG_LOG}" >&2 || true
    fi
    return 1
}

start_xvfb() {
    log "falling back to Xvfb on ${DISPLAY_NUM} (${SCREEN_WIDTH}x${SCREEN_HEIGHT}x${SCREEN_DEPTH})"
    rm -f /tmp/.X0-lock /tmp/.X11-unix/X0
    Xvfb "${DISPLAY_NUM}" -screen 0 "${SCREEN_WIDTH}x${SCREEN_HEIGHT}x${SCREEN_DEPTH}" -nolisten tcp &
    PIDS+=("$!")
    if wait_for_display 50; then
        DISPLAY_BACKEND="xvfb"
        return 0
    fi
    log "Xvfb failed to become ready"
    return 1
}

if ! start_dummy_xorg; then
    if [ "${FORCE_DUMMY}" = "1" ]; then
        log "FORCE_DUMMY=1 set; not falling back to Xvfb"
        exit 1
    fi
    # Drop the failed Xorg pid before starting Xvfb.
    if [ "${#PIDS[@]}" -gt 0 ]; then
        kill "${PIDS[-1]}" 2>/dev/null || true
        unset "PIDS[-1]"
    fi
    start_xvfb
fi

log "display backend=${DISPLAY_BACKEND}"
xdpyinfo -display "${DISPLAY_NUM}" | awk '/dimensions:/{print "start-desktop: "$0}' >&2 || true

xhost +local: >/dev/null

# gnome-session talks to the system bus. Docker / Cube images do not
# start systemd, so bring dbus-daemon --system up ourselves.
if [ ! -S /run/dbus/system_bus_socket ] && [ ! -S /var/run/dbus/system_bus_socket ]; then
    log "starting system dbus"
    mkdir -p /run/dbus /var/run/dbus
    dbus-uuidgen --ensure >/dev/null 2>&1 || true
    dbus-daemon --system --fork
fi

# gnome-shell's LoginManagerSystemd requires org.freedesktop.login1.
# systemd is already a GNOME dependency; logind can run without PID 1.
if ! pgrep -x systemd-logind >/dev/null 2>&1; then
    log "starting systemd-logind"
    mkdir -p /run/systemd/system
    /lib/systemd/systemd-logind &
    PIDS+=("$!")
    for i in $(seq 1 30); do
        if [ -S /run/systemd/private ] || pgrep -x systemd-logind >/dev/null 2>&1; then
            if gdbus introspect --system --dest org.freedesktop.login1 \
                --object-path /org/freedesktop/login1 >/dev/null 2>&1; then
                break
            fi
        fi
        sleep 0.2
    done
fi

# Ubuntu GNOME (Yaru + left dock). gnome-shell needs its own session bus
# as ${DESKTOP_USER}, plus software GL on dummy/Xvfb.
log "starting Ubuntu GNOME session as ${DESKTOP_USER}"
sudo -u "${DESKTOP_USER}" -H env \
    DISPLAY="${DISPLAY_NUM}" \
    HOME="/home/${DESKTOP_USER}" \
    XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR}" \
    XDG_SESSION_TYPE=x11 \
    XDG_CURRENT_DESKTOP=ubuntu:GNOME \
    GNOME_SHELL_SESSION_MODE=ubuntu \
    LIBGL_ALWAYS_SOFTWARE=1 \
    GALLIUM_DRIVER=llvmpipe \
    dbus-run-session -- gnome-session --session=ubuntu --disable-acceleration-check &
PIDS+=("$!")

for i in $(seq 1 50); do
    if pgrep -u "${DESKTOP_USER}" -x gnome-shell >/dev/null 2>&1; then
        log "gnome-shell is up"
        break
    fi
    sleep 0.3
done

apply_user_settings() {
    local shell_pid bus
    shell_pid="$(pgrep -u "${DESKTOP_USER}" -x gnome-shell | head -n1 || true)"
    [ -n "${shell_pid}" ] || return 0
    bus="$(tr '\0' '\n' < "/proc/${shell_pid}/environ" | sed -n 's/^DBUS_SESSION_BUS_ADDRESS=//p' | head -n1 || true)"
    [ -n "${bus}" ] || return 0
    sudo -u "${DESKTOP_USER}" -H env \
        DISPLAY="${DISPLAY_NUM}" \
        HOME="/home/${DESKTOP_USER}" \
        DBUS_SESSION_BUS_ADDRESS="${bus}" \
        gsettings set org.gnome.shell favorite-apps \
        "['org.gnome.Nautilus.desktop', 'org.gnome.Terminal.desktop']" || true
}

apply_user_settings

log "starting x11vnc on :${VNC_PORT}"
x11vnc \
    -display "${DISPLAY_NUM}" \
    -rfbport "${VNC_PORT}" \
    -forever \
    -shared \
    -nopw \
    -noxdamage \
    -noxfixes \
    -noxrandr \
    -o /var/log/x11vnc.log &
PIDS+=("$!")

for i in $(seq 1 40); do
    if ss -lnt 2>/dev/null | grep -q ":${VNC_PORT} "; then
        break
    fi
    sleep 0.25
done

log "starting noVNC on :${NOVNC_PORT} (web=${NOVNC_WEB})"
websockify --web="${NOVNC_WEB}" "${NOVNC_PORT}" "localhost:${VNC_PORT}" &
PIDS+=("$!")
NOVNC_PID="${PIDS[-1]}"

log "ready: backend=${DISPLAY_BACKEND} novnc=http://0.0.0.0:${NOVNC_PORT}/"
wait "${NOVNC_PID}"

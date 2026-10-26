# syntax=docker/dockerfile:1.7
#
# Ubuntu desktop sandbox: dummy Xorg :0 (1280x720) + Ubuntu GNOME
# (ubuntu-session / Yaru / left dock) + x11vnc + noVNC.
#
# Local Linux debug (same image Cube will consume):
#   ./run-local.sh
#   open http://127.0.0.1:6080/
#
# Cube template:
#   cubemastercli tpl create-from-image --image ... --probe 6080 --probe-path /

ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG DEBIAN_FRONTEND=noninteractive
# archive.ubuntu.com is often unusable from China and makes the GNOME
# install look hung for tens of minutes. Override with --build-arg APT_MIRROR=
# to use upstream, or another mirror.
ARG APT_MIRROR=http://mirrors.tencent.com/ubuntu

ENV DISPLAY=:0 \
    SCREEN_WIDTH=1280 \
    SCREEN_HEIGHT=720 \
    SCREEN_DEPTH=24 \
    VNC_PORT=5900 \
    NOVNC_PORT=6080 \
    DESKTOP_USER=user \
    LANG=C.UTF-8 \
    XDG_SESSION_TYPE=x11 \
    XDG_CURRENT_DESKTOP=ubuntu:GNOME \
    GNOME_SHELL_SESSION_MODE=ubuntu \
    LIBGL_ALWAYS_SOFTWARE=1 \
    GALLIUM_DRIVER=llvmpipe

RUN if [ -n "${APT_MIRROR}" ]; then \
        sed -i \
            -e "s|http://archive.ubuntu.com/ubuntu|${APT_MIRROR}|g" \
            -e "s|http://security.ubuntu.com/ubuntu|${APT_MIRROR}|g" \
            /etc/apt/sources.list; \
    fi \
    && apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=20 update \
    && apt-get -o Acquire::Retries=3 -y --no-install-recommends install \
        xserver-xorg-core \
        xserver-xorg-video-dummy \
        xserver-xorg-legacy \
        x11-xserver-utils \
        x11-utils \
        xvfb \
        dbus-x11 \
        dbus-user-session \
        ubuntu-session \
        ubuntu-settings \
        ubuntu-wallpapers \
        gnome-shell \
        gnome-session \
        gnome-terminal \
        nautilus \
        gnome-shell-extension-ubuntu-dock \
        gnome-shell-extension-desktop-icons-ng \
        yaru-theme-gtk \
        yaru-theme-icon \
        yaru-theme-gnome-shell \
        fonts-ubuntu \
        libgl1-mesa-dri \
        mesa-utils \
        librsvg2-common \
        libglib2.0-bin \
        at-spi2-core \
        x11vnc \
        novnc \
        websockify \
        python3 \
        python3-tk \
        python3-xlib \
        gnome-screenshot \
        wmctrl \
        ffmpeg \
        xclip \
        fonts-dejavu-core \
        sudo \
        iproute2 \
        procps \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN id -u user >/dev/null 2>&1 || useradd -m -s /bin/bash user \
    && echo 'user ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/user \
    && chmod 440 /etc/sudoers.d/user

COPY xorg.conf /etc/X11/xorg.conf
COPY Xwrapper.config /etc/X11/Xwrapper.config
COPY start-desktop.sh /usr/local/bin/start-desktop.sh
COPY novnc-index.html /usr/share/novnc/index.html
COPY 90-cube-ubuntu-desktop.gschema.override /usr/share/glib-2.0/schemas/90-cube-ubuntu-desktop.gschema.override

RUN glib-compile-schemas /usr/share/glib-2.0/schemas \
    && chmod 755 /usr/local/bin/start-desktop.sh

EXPOSE 6080 49983 5900

# Keep cubesandbox-base ENTRYPOINT (envd in the background).
CMD ["/usr/local/bin/start-desktop.sh"]

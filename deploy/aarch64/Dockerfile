FROM tencentos/tencentos4-init:latest
ENV LANG=C.UTF-8 \
    LC_ALL=C.UTF-8
RUN --mount=type=cache,target=/var/cache/dnf,sharing=locked \
    set -eux; \
    dnf install -yq epol-release; \
    dnf install -yq --setopt=install_weak_deps=False --setopt=tsflags=nodocs \
        'dnf-command(download)' \
        asciidoc asciidoctor autoconf automake bc \
        bcc-devel binutils-devel bison capstone-devel clang clang-devel \
        clang-tools-extra cmake cpio diffutils dwarves elfutils-devel \
        elfutils-libelf-devel findutils flex gcc gcc-c++ gettext \
        gettext-devel git glib2-static glibc-static gmock-devel \
        gtest-devel help2man java-devel jna libbabeltrace-devel \
        libbpf-devel libcap-devel libcap-static libcurl-devel libdwarf-static \
        libffi-devel libpcap-devel libpfm-devel libselinux-static \
        libsepol-static libsqlite3x-devel libtool libtraceevent-devel \
        libunwind-devel lld llvm llvm-devel llvm-libs llvm-libunwind-devel \
        llvm-libunwind-static make meson ncurses-devel numactl-devel \
        numactl-libs openssl-devel pcre2-static perl perl-ExtUtils-Embed \
        pixman-devel python3-devel python3-setuptools rpm-build rsync \
        slang-devel sysconftool systemtap-sdt-devel texinfo \
        vim-common xmlto xz-devel xz-static zlib-devel \
        zlib-static zstd lz4 wget python3-docutils audit-libs-devel \
        gcc-plugin-devel hmaccalc hostname libnl3-devel net-tools \
        newt-devel pciutils-devel nasm acpica-tools pigz \
        libmount-devel libyaml-devel e2fsprogs kmod \
        gcc-aarch64-linux-gnu binutils-aarch64-linux-gnu; \
    dnf clean all; \
    rm -rf /var/cache/yum/* /tmp/* /var/tmp/*
FROM tencentos/tencentos4-minimal:latest

WORKDIR /work

# Keep only the runtime essentials:
# - util-linux: mount/umount/blkid (referenced indirectly by agent)
# - busybox:    provides /bin/sh and core applets (awk/sed/grep, etc.).
#               Note: busybox applets are lighter-weight reimplementations
#               and do not cover every GNU extension; current guest startup
#               (cube-agent + empty rc.local) does not rely on those.
RUN yum install -y --setopt=install_weak_deps=False --setopt=tsflags=nodocs \
        util-linux \
        busybox \
 && yum clean all \
 && rm -rf /var/cache/yum /var/cache/dnf /var/log/yum.log /var/log/dnf.* \
           /usr/share/doc /usr/share/man /usr/share/info \
           /usr/share/licenses /usr/share/groff \
           /usr/share/locale/* /usr/lib/locale/* \
           /tmp/* /var/tmp/*

CMD ["bash"]

FROM cube-sandbox-image.tencentcloudcr.com/opensource/tencentos4-minimal:4.4-v20250331

WORKDIR /work

# Keep only the runtime essentials:
# - util-linux: mount/umount/blkid (referenced indirectly by agent)
# - busybox:    /bin/sh plus awk/sed/grep applets replacing gawk/which/etc.
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

#!/bin/bash

# Re-exec under bash if the script was invoked through /bin/sh. This keeps
# [[ ]] and pipefail working even on systems where /bin/sh is dash.
if [ -z "${BASH_VERSION:-}" ]; then
    if command -v bash >/dev/null 2>&1; then
        exec bash "$0" "$@"
    else
        echo "ERROR: this script requires bash, but bash was not found in PATH" >&2
        exit 1
    fi
fi

set -euo pipefail

GRUB_CMDLINE_LINUX_APPEND="quiet elevator=noop console=ttyS0,115200 console=tty0 vconsole.keymap=us \
crashkernel=1800M-64G:256M,64G-128G:512M,128G-486G:768M,486G-972G:1024M,972G-:2048M \
vconsole.font=latarcyrheb-sun16 net.ifnames=0 biosdevname=0 intel_idle.max_cstate=1 intel_pstate=disable cgroup.memory=nokmem transparent_hugepage=never \
ipv6.disable=1 systemd.unified_cgroup_hierarchy=1 module.sig_enforce=1 \
clearcpuid=27,28,54,57,104,107,118,120,122,131,152,158,193,196,198,199,200,201,214,215,225,241,249,250,254,289,292,295,297,299,302,306,307,309,311,312,317,321,322,323,389,416,418,425,513,514,517,518,520,521,522,523,524,526,534,537,539,540,580 \
clocksource=tsc pti=off no5lvl mitigations=on spec_store_bypass_disable=prctl retbleed=off \
kvm.nx_huge_pages=never tsc=reliable kmem_cache.max_num=16000"

[[ $EUID -eq 0 ]] || {
    echo "please run as root" >&2
    exit 1
}

cp -a /etc/default/grub /etc/default/grub.bak.$(date +%s)

# Read the existing GRUB_CMDLINE_LINUX value from /etc/default/grub,
# or treat it as empty if it is not present.
existing_cmdline=""
if grep -qE '^GRUB_CMDLINE_LINUX=' /etc/default/grub; then
    existing_cmdline=$(grep -E '^GRUB_CMDLINE_LINUX=' /etc/default/grub | head -n1 \
        | sed -E 's/^GRUB_CMDLINE_LINUX=//; s/^"(.*)"$/\1/; s/^'\''(.*)'\''$/\1/')
fi

# Merge the appended parameters into the existing cmdline. De-duplicate by
# key, and let appended parameters override existing parameters with the same key.
merged_cmdline="$existing_cmdline"
for param in $GRUB_CMDLINE_LINUX_APPEND; do
    key="${param%%=*}"
    # Drop existing parameters with the same key, in either key or key=value form.
    merged_cmdline=$(printf '%s\n' "$merged_cmdline" | tr -s ' ' '\n' \
        | awk -v key="$key" 'NF { split($0, parts, "="); if (parts[1] != key) print }' \
        | tr '\n' ' ')
    merged_cmdline="${merged_cmdline% } $param"
done
# Normalize extra whitespace.
merged_cmdline=$(echo "$merged_cmdline" | tr -s ' ' | sed -E 's/^ +//; s/ +$//')

if grep -qE '^GRUB_CMDLINE_LINUX=' /etc/default/grub; then
    sed -i "/^GRUB_CMDLINE_LINUX=/c\\GRUB_CMDLINE_LINUX=\"$merged_cmdline\"" /etc/default/grub
else
    echo "GRUB_CMDLINE_LINUX=\"$merged_cmdline\"" >> /etc/default/grub
fi

if command -v update-grub >/dev/null 2>&1; then
    # Ubuntu / Debian
    update-grub
elif command -v grub2-mkconfig >/dev/null 2>&1; then
    # CentOS / RHEL / TencentOS
    if [[ -d /boot/grub2 ]]; then
        grub2-mkconfig -o /boot/grub2/grub.cfg
    else
        grub2-mkconfig -o /boot/grub/grub.cfg
    fi
elif command -v grub-mkconfig >/dev/null 2>&1; then
    grub-mkconfig -o /boot/grub/grub.cfg
else
    echo "ERROR: no grub config tool found (update-grub / grub2-mkconfig / grub-mkconfig)" >&2
    exit 1
fi

#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Run with: bash deploy/pvm/grub/test-host-grub-config.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# An optional source path supports testing alternate implementations.
ENTRY="${1:-${SCRIPT_DIR}/host_grub_config.sh}"

# Load the production function without root checks, /etc writes or GRUB commands.
# shellcheck source=host_grub_config.sh
source "$ENTRY"

check_merge() {
    local name="$1" existing="$2" append="$3" expected="$4"
    local actual repeated
    actual="$(merge_cmdline "$existing" "$append")"
    if [[ "$actual" != "$expected" ]]; then
        printf 'FAIL: %s\n  expected: %s\n  actual:   %s\n' \
            "$name" "$expected" "$actual" >&2
        exit 1
    fi
    repeated="$(merge_cmdline "$actual" "$append")"
    if [[ "$repeated" != "$actual" ]]; then
        printf 'FAIL: %s is not idempotent\n  first:  %s\n  second: %s\n' \
            "$name" "$actual" "$repeated" >&2
        exit 1
    fi
    printf 'ok: %s (including repeated merge)\n' "$name"
}

check_merge 'preserve both console values with an empty existing cmdline' \
    '' 'console=ttyS0,115200 console=tty0' \
    'console=ttyS0,115200 console=tty0'

check_merge 'replace all old console values and preserve unrelated parameters' \
    'root=/dev/vda1 console=ttyS1 console=tty1 audit=1' \
    'console=ttyS0,115200 console=tty0' \
    'root=/dev/vda1 audit=1 console=ttyS0,115200 console=tty0'

check_merge 'preserve interleaved repeated keys in configured order' \
    'console=old audit=0' \
    'console=ttyS0,115200 audit=1 console=tty0' \
    'console=ttyS0,115200 audit=1 console=tty0'

check_merge 'match complete keys rather than prefixes' \
    'foobar=old foo=old' \
    'foobar=first foo=one foobar=second foo=two' \
    'foobar=first foo=one foobar=second foo=two'

check_merge 'replace both bare and valued occurrences of a key' \
    'quiet=0 quiet root=/dev/vda1' 'quiet' 'root=/dev/vda1 quiet'

check_merge 'preserve explicitly repeated identical arguments' \
    'console=old' 'console=tty0 console=tty0' 'console=tty0 console=tty0'

check_merge 'preserve existing parameters when nothing is appended' \
    'root=/dev/vda1 console=ttyS0,115200 console=tty0' '' \
    'root=/dev/vda1 console=ttyS0,115200 console=tty0'

# Matching filenames must not change the meaning of literal kernel arguments.
(
    fixture_dir="$(mktemp -d)"
    trap 'rm -rf "$fixture_dir"' EXIT
    cd "$fixture_dir"
    touch 'glob=matched' 'single=x' 'class=a'
    check_merge 'preserve literal glob characters despite matching filenames' \
        'root=/dev/vda1 glob=old single=old class=old' \
        'glob=* single=? class=[ab]' \
        'root=/dev/vda1 glob=* single=? class=[ab]'

    options_before="$-"
    ifs_before="$IFS"
    merge_cmdline '' 'glob=*' >/dev/null
    if [[ "$-" != "$options_before" || "$IFS" != "$ifs_before" ]]; then
        printf 'FAIL: merge_cmdline changed caller shell options or IFS\n' >&2
        exit 1
    fi
    printf 'ok: merge_cmdline preserves caller shell options and IFS\n'
)

# Use the same defaults as main, so changes to the actual configuration are
# covered without duplicating the parameter list in this test.
default_append="$(default_grub_cmdline)"
check_merge 'preserve the full production configuration' \
    '' "$default_append" "$default_append"

check_merge 'replace old consoles with production defaults and preserve root' \
    'root=/dev/vda1 console=ttyS1 console=tty1' "$default_append" \
    "root=/dev/vda1 $default_append"

production_cmdline="$(merge_cmdline '' "$default_append")"
read -r -a production_params <<< "$production_cmdline"
consoles=()
for param in "${production_params[@]}"; do
    if [[ "$param" == console=* ]]; then
        consoles+=("$param")
    fi
done
if [[ "${consoles[*]}" != 'console=ttyS0,115200 console=tty0' ]]; then
    printf 'FAIL: production console values or order changed: %s\n' \
        "${consoles[*]}" >&2
    exit 1
fi
printf 'ok: production defaults retain both intended consoles in order\n'

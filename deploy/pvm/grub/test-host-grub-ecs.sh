#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Execute the unmodified deployment script as root in a disposable container.
# Only update-grub is stubbed; no host boot configuration is mounted.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRY="${1:-${SCRIPT_DIR}/host_grub_config.sh}"
EXPECTED="${2:-${SCRIPT_DIR}/fixtures/ecs-fixed.grub}"
ENTRY="$(cd "$(dirname "$ENTRY")" && pwd)/$(basename "$ENTRY")"
EXPECTED="$(cd "$(dirname "$EXPECTED")" && pwd)/$(basename "$EXPECTED")"

docker run --rm --network none \
    -v "$ENTRY:/test/script.sh:ro" \
    -v "$EXPECTED:/test/expected.grub:ro" \
    -v "${SCRIPT_DIR}/fixtures/ecs-original.grub:/test/original.grub:ro" \
    ubuntu:24.04 bash -euo pipefail -c '
        mkdir -p /etc/default
        cp /test/original.grub /etc/default/grub
        printf "#!/bin/sh\necho called >> /tmp/update-grub.calls\n" > /usr/local/bin/update-grub
        chmod +x /usr/local/bin/update-grub
        bash /test/script.sh
        cmp /test/expected.grub /etc/default/grub
        backups=(/etc/default/grub.bak.*)
        test "${#backups[@]}" -eq 1
        cmp /test/original.grub "${backups[0]}"
        test "$(wc -l < /tmp/update-grub.calls)" -eq 1
        bash /test/script.sh
        cmp /test/expected.grub /etc/default/grub
        test "$(wc -l < /tmp/update-grub.calls)" -eq 2
        echo "ok: full ECS configuration, original backup, update-grub call, and idempotence"
    '

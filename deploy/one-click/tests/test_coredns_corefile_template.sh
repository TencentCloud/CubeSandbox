#!/usr/bin/env bash
# Static guard for deploy/one-click/coredns/Corefile.template.
#
# cube.app / *.cube.app are synthetic IPv4-only zones served by the local
# CoreDNS. Their AAAA queries must be answered locally with NODATA (an empty
# NOERROR): if AAAA falls through to `forward . /etc/resolv.conf`, every new
# client connection on a host with an unreachable upstream resolver stalls for
# the full forward timeout (2s per attempt, 4s with a retry). This guards the
# fix from PR #2075 against a future template edit silently reverting it,
# including the subtler regressions: growing an `answer` (a fabricated AAAA
# such as ::1 wins RFC 6724 sorting on dual-stack clients) or a `fallthrough`
# (re-opens the forward path).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
TEMPLATE="${ONE_CLICK_DIR}/coredns/Corefile.template"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

[[ -f "${TEMPLATE}" ]] || fail "missing ${TEMPLATE}"

# Both synthetic-zone AAAA stanzas must exist (literal lines).
grep -Fq 'template IN AAAA cube.app {' "${TEMPLATE}" \
  || fail "missing 'template IN AAAA cube.app' stanza"
grep -Fq 'template IN AAAA (.*)\.cube\.app {' "${TEMPLATE}" \
  || fail "missing wildcard 'template IN AAAA' stanza"

# Every AAAA stanza must be terminal NODATA: rcode NOERROR inside, and no
# answer record / no fallthrough (either would reintroduce the stall or,
# worse, hand clients a fabricated AAAA to connect to).
report="$(awk '
  /^[[:space:]]*template IN AAAA/ { in_block = 1; blocks++; next }
  in_block && /^[[:space:]]*\}/   { in_block = 0; next }
  in_block {
    if ($0 ~ /answer/)      printf "  AAAA stanza has an answer record: %s\n", $0
    if ($0 ~ /fallthrough/) printf "  AAAA stanza has fallthrough: %s\n", $0
    if ($0 ~ /rcode[[:space:]]+NOERROR/) nodata++
  }
  END {
    if (blocks != 2)      printf "  expected 2 AAAA stanzas, found %d\n", blocks + 0
    if (nodata != blocks) printf "  rcode NOERROR missing in an AAAA stanza (%d/%d)\n", nodata + 0, blocks + 0
  }
' "${TEMPLATE}")"
if [[ -n "${report}" ]]; then
  echo "FAIL: Corefile.template AAAA regression:" >&2
  echo "${report}" >&2
  exit 1
fi

# The A stanzas must stay intact: both answer with the rendered node IP.
grep -Fq 'template IN A cube.app {' "${TEMPLATE}" \
  || fail "missing 'template IN A cube.app' stanza"
grep -Fq 'template IN A (.*)\.cube\.app {' "${TEMPLATE}" \
  || fail "missing wildcard 'template IN A' stanza"
[[ "$(grep -c '__CUBE_PROXY_DNS_ANSWER_IP__' "${TEMPLATE}")" -eq 2 ]] \
  || fail "both A stanzas must answer with __CUBE_PROXY_DNS_ANSWER_IP__"

echo "PASS: Corefile.template answers cube.app AAAA with terminal NODATA"

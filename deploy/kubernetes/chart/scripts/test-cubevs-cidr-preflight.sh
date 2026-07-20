#!/usr/bin/env bash
# Unit tests for cubevs-cidr-preflight.sh overlap helpers (no cluster required).
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
PREFLIGHT="$CHART_DIR/files/cubevs-cidr-preflight.sh"

CUBEVS_CIDR_PREFLIGHT_SOURCE_ONLY=1
# shellcheck disable=SC1090
. "$PREFLIGHT"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_overlaps() {
  if ! cidr_overlaps "$1" "$2"; then
    fail "expected overlap: $1 vs $2"
  fi
}

assert_no_overlap() {
  if cidr_overlaps "$1" "$2"; then
    fail "expected no overlap: $1 vs $2"
  fi
}

assert_overlaps "172.16.0.0/18" "172.16.0.0/18"
assert_overlaps "192.168.0.0/18" "192.168.0.0/16"
assert_overlaps "192.168.0.0/18" "192.168.34.103/32"
assert_no_overlap "172.16.0.0/18" "192.168.0.0/16"
assert_no_overlap "172.16.0.0/18" "10.96.0.0/12"
assert_no_overlap "172.16.0.0/18" "192.168.34.103/32"

if ! ip_in_cidr "192.168.34.103" "192.168.0.0/18"; then
  fail "192.168.34.103 should be in 192.168.0.0/18"
fi
if ip_in_cidr "192.168.34.103" "172.16.0.0/18"; then
  fail "192.168.34.103 must not be in 172.16.0.0/18"
fi

validate_cidr "172.16.0.0/18"

if ( validate_cidr "172.16.0.1/18" ) >/dev/null 2>&1; then
  fail "misaligned CIDR should be rejected"
fi

if ip_to_int "1.2.3" >/dev/null 2>&1; then
  fail "truncated IP 1.2.3 must be rejected"
fi
if ip_to_int "1.2.3.4.5" >/dev/null 2>&1; then
  fail "overlong IP 1.2.3.4.5 must be rejected"
fi
if cidr_range "1.2.3/18" >/dev/null 2>&1; then
  fail "cidr_range must reject truncated IP"
fi
if cidr_overlaps "1.2.3/18" "172.16.0.0/18" >/dev/null 2>&1; then
  fail "cidr_overlaps must reject truncated IP"
fi

echo "cubevs-cidr-preflight unit tests passed"

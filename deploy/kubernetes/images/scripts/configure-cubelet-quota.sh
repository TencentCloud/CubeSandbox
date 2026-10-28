#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
# Apply Helm quota overrides to the writable, staged dynamicconf before startup.
# The image ships block-style YAML; scope replacements to host.quota so other
# config sections and values not supplied by the operator remain unchanged.
set -euo pipefail

config="${1:?usage: configure-cubelet-quota.sh DYNAMICCONF}"
keys=(mcpu_limit mem_limit mvm_limit creation_concurrent_num paused_resource_release_ratio)
present=false
applied=()
for key in "${keys[@]}"; do
  name="CUBELET_QUOTA_${key^^}"
  [[ -v "$name" ]] || continue
  present=true
  value="${!name}"
  case "$key" in
    mem_limit)
      pattern='^$|^[+]?([0-9]+([.][0-9]*)?|[.][0-9]+)([eE][+-]?[0-9]+|[numkMGTP]|[KMGTPE]i|E)?$'
      ;;
    paused_resource_release_ratio)
      pattern='^[+]?([0-9]+([.][0-9]*)?|[.][0-9]+)([eE][+-]?[0-9]+)?$'
      ;;
    *) pattern='^(0|[1-9][0-9]*)$' ;;
  esac
  if ! [[ "$value" =~ $pattern ]] ||
    { [[ "$key" == paused_resource_release_ratio ]] &&
      ! awk -v value="$value" 'BEGIN { exit !(value >= 0 && value <= 1) }'; }; then
    echo "invalid $name" >&2
    exit 1
  fi
  applied+=("$key=\"$value\"")
done
"$present" || exit 0

tmp="$(mktemp "${config}.quota.XXXXXX")"
trap 'rm -f -- "$tmp"' EXIT
awk '
  BEGIN {
    count = split("mcpu_limit mem_limit mvm_limit creation_concurrent_num paused_resource_release_ratio", keys, " ")
    for (i = 1; i <= count; i++) {
      key = keys[i]
      name = "CUBELET_QUOTA_" toupper(key)
      if (name in ENVIRON) {
        supplied[key] = 1
        value[key] = ENVIRON[name]
        if (key == "mem_limit") value[key] = "\"" value[key] "\""
      }
    }
  }
  function finish_quota(    j, k) {
    for (j = 1; j <= count; j++) {
      k = keys[j]
      if (supplied[k] && !seen[k]) print "    " k ": " value[k]
    }
    in_quota = 0
  }
  {
    if (in_quota && /^[^#[:space:]]|^  [^#[:space:]]/) finish_quota()
    if (in_quota && !/^[[:space:]]*(#.*)?$/ && !/^    [a-z_]+:/) {
      print "unsupported host.quota layout: expected four-space field indentation" > "/dev/stderr"
      exit 1
    }
    if (/^[^#[:space:]]/) in_host = ($0 ~ /^host:[[:space:]]*(#.*)?$/)
    if (in_host && /^  quota:/ && !/^  quota:[[:space:]]*(#.*)?$/) {
      print "unsupported host.quota layout: expected a block mapping" > "/dev/stderr"
      exit 1
    }
    if (in_host && /^  quota:[[:space:]]*(#.*)?$/) {
      in_quota = 1
      found++
    }
    if (in_quota && /^    [a-z_]+:/) {
      key = $1
      sub(/:$/, "", key)
      if (supplied[key]) {
        print "    " key ": " value[key]
        seen[key] = 1
        next
      }
    }
    print
  }
  END {
    if (in_quota) finish_quota()
    if (found != 1) {
      print "expected one host.quota block in staged dynamicconf" > "/dev/stderr"
      exit 1
    }
  }
' "$config" > "$tmp"
chmod --reference="$config" "$tmp"
mv -f -- "$tmp" "$config"
printf 'cubelet quota overrides applied: %s\n' "${applied[*]}" >&2

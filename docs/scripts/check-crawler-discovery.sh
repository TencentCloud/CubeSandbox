#!/usr/bin/env bash
# Verify crawler discovery files after `npm run docs:build`.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="${ROOT}/.vitepress/dist"
ORIGIN="https://cubesandbox.com"
failed=0

fail() {
  echo "::error::$1"
  failed=1
}

require_file() {
  local path="$1"
  if [[ ! -f "${path}" ]]; then
    fail "Missing ${path#"${DIST}"/}"
  fi
}

require_file "${DIST}/robots.txt"
require_file "${DIST}/sitemap.xml"
require_file "${DIST}/llms.txt"
require_file "${DIST}/llms-full.txt"

if [[ -f "${DIST}/robots.txt" ]]; then
  if ! grep -q "Sitemap: ${ORIGIN}/sitemap.xml" "${DIST}/robots.txt"; then
    fail "robots.txt must declare Sitemap: ${ORIGIN}/sitemap.xml"
  fi
fi

if [[ -f "${DIST}/sitemap.xml" ]]; then
  if ! grep -q "${ORIGIN}/guide/quickstart" "${DIST}/sitemap.xml"; then
    fail "sitemap.xml must include ${ORIGIN}/guide/quickstart"
  fi
  if grep -q "${ORIGIN}/guide/quickstart.html" "${DIST}/sitemap.xml"; then
    fail "sitemap.xml must use clean URLs (found /guide/quickstart.html)"
  fi
  if grep -Eq "${ORIGIN}/404(\.html)?<" "${DIST}/sitemap.xml"; then
    fail "sitemap.xml must not include the 404 page"
  fi
fi

if [[ -f "${DIST}/llms.txt" ]]; then
  if ! head -n 1 "${DIST}/llms.txt" | grep -qx '# CubeSandbox'; then
    fail "llms.txt must start with '# CubeSandbox'"
  fi
  if ! grep -qx '## English' "${DIST}/llms.txt"; then
    fail "llms.txt must have an English section"
  fi
  if ! grep -qx '## 简体中文' "${DIST}/llms.txt"; then
    fail "llms.txt must have a Chinese section"
  fi

  while IFS= read -r url; do
    [[ -n "${url}" ]] || continue
    rel="${url#"${ORIGIN}/"}"
    if [[ ! -f "${DIST}/${rel}" ]]; then
      fail "llms.txt link missing from dist: ${rel}"
    fi
  done < <(grep -oE "${ORIGIN}/[^)[:space:]]+\.md" "${DIST}/llms.txt")

  mapfile -t en_pins < <(awk '/^## English/{p=1; next} /^## /{p=0} p && /^- \[/{print}' "${DIST}/llms.txt" | head -n 5)
  mapfile -t zh_pins < <(awk '/^## 简体中文/{p=1; next} /^## /{p=0} p && /^- \[/{print}' "${DIST}/llms.txt" | head -n 5)
  expected_en=(
    "${ORIGIN}/guide/introduction.md"
    "${ORIGIN}/guide/quickstart.md"
    "${ORIGIN}/guide/pvm-deploy.md"
    "${ORIGIN}/guide/bare-metal-deploy.md"
    "${ORIGIN}/guide/kubernetes.md"
  )
  expected_zh=(
    "${ORIGIN}/zh/guide/introduction.md"
    "${ORIGIN}/zh/guide/quickstart.md"
    "${ORIGIN}/zh/guide/pvm-deploy.md"
    "${ORIGIN}/zh/guide/bare-metal-deploy.md"
    "${ORIGIN}/zh/guide/kubernetes.md"
  )
  for i in "${!expected_en[@]}"; do
    if [[ "${en_pins[$i]}" != *"${expected_en[$i]}"* ]]; then
      fail "llms.txt English pin $((i + 1)) should be ${expected_en[$i]}"
    fi
    if [[ "${zh_pins[$i]}" != *"${expected_zh[$i]}"* ]]; then
      fail "llms.txt Chinese pin $((i + 1)) should be ${expected_zh[$i]}"
    fi
  done
fi

if [[ -f "${DIST}/llms-full.txt" ]]; then
  mapfile -t full_urls < <(grep -oE "^url: ${ORIGIN}/[^[:space:]]+" "${DIST}/llms-full.txt" | sed 's/^url: //')
  expected_full=(
    "${ORIGIN}/guide/introduction.md"
    "${ORIGIN}/guide/quickstart.md"
    "${ORIGIN}/guide/pvm-deploy.md"
    "${ORIGIN}/guide/bare-metal-deploy.md"
    "${ORIGIN}/guide/kubernetes.md"
  )
  for i in "${!expected_full[@]}"; do
    if [[ "${full_urls[$i]}" != "${expected_full[$i]}" ]]; then
      fail "llms-full.txt pin $((i + 1)) should be ${expected_full[$i]}"
    fi
  done
fi

if [[ -f "${DIST}/sitemap.xml" ]]; then
  while IFS= read -r loc; do
    [[ -n "${loc}" ]] || continue
    path="${loc#"${ORIGIN}"}"
    path="${path#/}"
    path="${path%/}"
    if [[ -z "${path}" ]]; then
      md="index.md"
    elif [[ -f "${DIST}/${path}.md" ]]; then
      md="${path}.md"
    elif [[ -f "${DIST}/${path}/index.md" ]]; then
      md="${path}/index.md"
    else
      fail "sitemap URL has no sibling markdown: /${path}"
      continue
    fi
  done < <(grep -oE "<loc>${ORIGIN}[^<]*</loc>" "${DIST}/sitemap.xml" | sed -E "s#</?loc>##g")
fi

for sample in \
  guide/quickstart.md \
  zh/guide/quickstart.md \
  guide/kubernetes.md \
  zh/guide/introduction.md
do
  require_file "${DIST}/${sample}"
done

if [[ "${failed}" -ne 0 ]]; then
  echo "Crawler discovery checks failed."
  exit 1
fi

echo "Crawler discovery checks passed."

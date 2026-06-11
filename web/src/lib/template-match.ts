// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.
//
// Shared template-matching logic used by both TemplateStore and SandboxCases.
// The key insight: `imageInfo` from the API often contains a digest suffix
// (e.g. `registry/path/image:latest@sha256:abc123`), while the catalog's
// `image_cn` only stores the base ref (`registry/path/image:latest`).
// Strict equality (===) will always fail; we must use `includes()` matching
// — the same strategy the TemplateStore page already uses.

import type { StoreCatalogItem, TemplateSummary } from '@/api/client';

/** Return all READY templates whose image matches the given catalog item. */
export function getInstalledTemplates(
  item: StoreCatalogItem,
  templates: TemplateSummary[],
): TemplateSummary[] {
  return templates.filter((tpl) => {
    if (!tpl.imageInfo) return false;
    const statusOk = tpl.status?.toUpperCase() === 'READY';
    if (!statusOk) return false;
    if (item.digest && tpl.imageInfo.includes(item.digest)) return true;
    const imageName = item.image.split('@')[0];
    return tpl.imageInfo.includes(imageName);
  });
}

/** Return all BUILDING / PENDING templates whose image matches the catalog item. */
export function getBuildingTemplates(
  item: StoreCatalogItem,
  templates: TemplateSummary[],
): TemplateSummary[] {
  return templates.filter((tpl) => {
    if (!tpl.imageInfo) return false;
    const s = tpl.status?.toUpperCase();
    if (s !== 'BUILDING' && s !== 'PENDING') return false;
    if (item.digest && tpl.imageInfo.includes(item.digest)) return true;
    const imageName = item.image.split('@')[0];
    return tpl.imageInfo.includes(imageName);
  });
}

/** Tri-state result for template match status. */
export type TemplateMatchStatus =
  | { kind: 'ready'; templates: TemplateSummary[] }
  | { kind: 'building'; templates: TemplateSummary[] }
  | { kind: 'not_installed' };

/**
 * Determine the match status for a catalog item against the current template list.
 * Returns the first applicable state in priority order: ready > building > not_installed.
 */
export function getTemplateMatchStatus(
  item: StoreCatalogItem,
  templates: TemplateSummary[],
): TemplateMatchStatus {
  const installed = getInstalledTemplates(item, templates);
  if (installed.length > 0) {
    return { kind: 'ready', templates: installed };
  }
  const building = getBuildingTemplates(item, templates);
  if (building.length > 0) {
    return { kind: 'building', templates: building };
  }
  return { kind: 'not_installed' };
}

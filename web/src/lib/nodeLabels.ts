// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

/**
 * Node label classification, mirroring the CubeOps label contract. The three
 * protection rules are not the same set, so the UI must not collapse them into
 * one "reserved" bucket:
 *
 *  - data-plane labels (`kubernetes.io/*`, `beta.kubernetes.io/*`,
 *    `cube.cloud.tencentcloud.com/*`) are reported by Cubelet and read by the
 *    scheduler: read-only through the label API, kept by a bulk replace.
 *  - the isolation label belongs to the isolation operation, not the label API.
 *  - `machine-type` is reserved on the generic label API; membership is changed
 *    through the machine-type catalog.
 *
 * Sources of truth (keep in sync):
 *   CubeOps/internal/nodemanagement/service/labels.go    systemLabelPrefixes,
 *                                                        replaceProtectedLabelKeys,
 *                                                        cubeletProtectedLabelKeys
 *   CubeOps/internal/nodemanagement/model/constants.go   LabelMachineType,
 *                                                        LabelSchedulingDisabled
 */

export const MACHINE_TYPE_KEY = 'cube.cubesandbox.io/machine-type';
export const ISOLATION_KEY = 'cube.cloud.tencentcloud.com/scheduling-disabled';

const SYSTEM_LABEL_PREFIXES = [
  'kubernetes.io',
  'beta.kubernetes.io',
  'cube.cloud.tencentcloud.com',
] as const;

/** Data-plane labels: reported by Cubelet, read-only for the admin API. */
export function isSystemLabelKey(key: string): boolean {
  const namespace = key.includes('/') ? key.slice(0, key.indexOf('/')) : key;
  return SYSTEM_LABEL_PREFIXES.some(
    (prefix) => namespace === prefix || namespace.endsWith(`.${prefix}`),
  );
}

export function isMachineTypeKey(key: string): boolean {
  return key === MACHINE_TYPE_KEY;
}

/** Only the isolation operation may write this key. */
export function isIsolationKey(key: string): boolean {
  return key === ISOLATION_KEY;
}

/**
 * Data-plane labels are read-only for the admin API. The isolation key is
 * deliberately NOT part of this set: it is maintained by the isolate/unisolate
 * actions on the same page.
 */
export function isReadOnlyLabelKey(key: string): boolean {
  return !isIsolationKey(key) && isSystemLabelKey(key);
}

/** Keys a bulk replace keeps at their stored value. */
export function isReplaceProtectedKey(key: string): boolean {
  return isIsolationKey(key) || isMachineTypeKey(key) || isSystemLabelKey(key);
}

/** Labels an operator manages: shown as editable in the UI. */
export function userLabelsOf(labels: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(labels)) {
    if (isReplaceProtectedKey(key)) continue;
    out[key] = value;
  }
  return out;
}

/** Data-plane labels, shown in the read-only group. */
export function readOnlyLabelsOf(labels: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(labels)) {
    if (!isReadOnlyLabelKey(key)) continue;
    out[key] = value;
  }
  return out;
}

export interface LabelDiff {
  added: string[];
  changed: string[];
  removed: string[];
  unchanged: string[];
}

/**
 * Compares the current operator labels against the edited set. `next` must not
 * contain protected keys, so the diff describes exactly what replace does.
 */
export function diffLabels(
  current: Record<string, string>,
  next: Record<string, string>,
): LabelDiff {
  const added: string[] = [];
  const changed: string[] = [];
  const removed: string[] = [];
  const unchanged: string[] = [];

  for (const key of Object.keys(next)) {
    if (!Object.hasOwn(current, key)) {
      added.push(key);
    } else if (current[key] !== next[key]) {
      changed.push(key);
    } else {
      unchanged.push(key);
    }
  }
  for (const key of Object.keys(current)) {
    if (!Object.hasOwn(next, key)) removed.push(key);
  }

  const sorted = (keys: string[]) => keys.sort((a, b) => a.localeCompare(b));
  return {
    added: sorted(added),
    changed: sorted(changed),
    removed: sorted(removed),
    unchanged: sorted(unchanged),
  };
}

const MACHINE_TYPE_VALUE = /^[a-z0-9][a-z0-9-]{0,62}$/;

/**
 * Machine-type value validation. Returns a translation key when invalid; the
 * shape matches the backend rule in `ValidateMachineTypeValue`.
 */
export function validateMachineTypeValue(value: string): 'required' | 'shape' | null {
  const trimmed = value.trim();
  if (!trimmed) return 'required';
  if (!MACHINE_TYPE_VALUE.test(trimmed)) return 'shape';
  return null;
}

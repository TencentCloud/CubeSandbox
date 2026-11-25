// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

/**
 * Guards the client-side label rules. `userLabelsOf` + `isReplaceProtectedKey`
 * are the only thing standing between the operator and a replace payload the
 * server rejects (machine-type) or silently ignores, so they get direct tests
 * instead of relying on the server-side e2e suite.
 *
 * Run with: npm run test
 */

import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  ISOLATION_KEY,
  MACHINE_TYPE_KEY,
  diffLabels,
  isReadOnlyLabelKey,
  isReplaceProtectedKey,
  isSystemLabelKey,
  readOnlyLabelsOf,
  userLabelsOf,
  validateMachineTypeValue,
} from './nodeLabels.ts';

test('data-plane namespaces are system labels', () => {
  for (const key of [
    'kubernetes.io/arch',
    'beta.kubernetes.io/os',
    'cube.cloud.tencentcloud.com/instance-type',
  ]) {
    assert.equal(isSystemLabelKey(key), true, key);
  }
  for (const key of [MACHINE_TYPE_KEY, 'team', 'example.com/team']) {
    assert.equal(isSystemLabelKey(key), false, key);
  }
});

test('system labels are read-only but machine-type is not a data-plane label', () => {
  assert.equal(isReadOnlyLabelKey('kubernetes.io/arch'), true);
  // machine-type has its own catalog UI; it is not shown in the data-plane group.
  assert.equal(isReadOnlyLabelKey(MACHINE_TYPE_KEY), false);
  // isolation has its own action; it is not part of the data-plane group.
  assert.equal(isReadOnlyLabelKey(ISOLATION_KEY), false);
});

test('replace keeps machine-type and isolation', () => {
  assert.equal(isReplaceProtectedKey(MACHINE_TYPE_KEY), true);
  assert.equal(isReplaceProtectedKey(ISOLATION_KEY), true);
  assert.equal(isReplaceProtectedKey('kubernetes.io/arch'), true);
  assert.equal(isReplaceProtectedKey('team'), false);
});

test('machine-type never enters the replace payload', () => {
  const labels = {
    [MACHINE_TYPE_KEY]: 'intel-k66',
    [ISOLATION_KEY]: 'true',
    'kubernetes.io/arch': 'amd64',
    team: 'infra',
  };
  assert.deepEqual(userLabelsOf(labels), { team: 'infra' });
  assert.deepEqual(readOnlyLabelsOf(labels), { 'kubernetes.io/arch': 'amd64' });
});

test('diff reports added, changed and removed keys', () => {
  const diff = diffLabels(
    { keep: 'v', same: '1', gone: 'x' },
    { keep: 'v2', same: '1', added: 'y' },
  );
  assert.deepEqual(diff.added, ['added']);
  assert.deepEqual(diff.changed, ['keep']);
  assert.deepEqual(diff.removed, ['gone']);
  assert.deepEqual(diff.unchanged, ['same']);
});

test('diff of an identical set is empty', () => {
  const diff = diffLabels({ a: '1' }, { a: '1' });
  assert.deepEqual([diff.added, diff.changed, diff.removed], [[], [], []]);
});

test('machine-type value shape matches the server rule', () => {
  for (const ok of ['intel-k66', 'a', 'amd-epyc-9k84']) {
    assert.equal(validateMachineTypeValue(ok), null, ok);
  }
  assert.equal(validateMachineTypeValue(''), 'required');
  assert.equal(validateMachineTypeValue('   '), 'required');
  for (const bad of ['-leading', 'Intel', 'intel_k66', 'a'.repeat(64)]) {
    assert.equal(validateMachineTypeValue(bad), 'shape', bad);
  }
  assert.equal(validateMachineTypeValue('  intel-k66  '), null);
});

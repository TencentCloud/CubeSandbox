// Locale consistency check.
//
// Every namespace JSON under src/locales/en must have a src/locales/zh
// counterpart (and vice versa), and the two files must expose the same set of
// flattened dotted keys. Asymmetry is reported in both directions:
//
//   key only in zh -> the English UI can only render the raw key (most severe)
//   key only in en -> the Chinese UI silently falls back to English
//
// Exits 1 when any namespace is asymmetric so CI fails loudly; prints a clear
// success line and exits 0 otherwise. Dependency-free Node ESM.

import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const LOCALES = ['en', 'zh'];
const localesDir = join(dirname(fileURLToPath(import.meta.url)), '..', 'src', 'locales');

/** List the JSON namespace files for a locale, sorted for stable output. */
function listNamespaces(locale) {
  return readdirSync(join(localesDir, locale))
    .filter((name) => name.endsWith('.json'))
    .sort();
}

/** Read and parse one namespace file for a locale. */
function readNamespace(locale, name) {
  return JSON.parse(readFileSync(join(localesDir, locale, name), 'utf8'));
}

/** Recursively flatten an object into dotted key paths. */
function flattenKeys(value, prefix = '') {
  const keys = [];
  for (const [key, child] of Object.entries(value)) {
    const path = prefix ? `${prefix}.${key}` : key;
    if (child !== null && typeof child === 'object' && !Array.isArray(child)) {
      keys.push(...flattenKeys(child, path));
    } else {
      keys.push(path);
    }
  }
  return keys;
}

const namespaces = Object.fromEntries(LOCALES.map((locale) => [locale, listNamespaces(locale)]));
const allNamespaces = [...new Set(LOCALES.flatMap((locale) => namespaces[locale]))].sort();

const findings = [];
for (const name of allNamespaces) {
  const missingIn = LOCALES.filter((locale) => !namespaces[locale].includes(name));
  if (missingIn.length > 0) {
    findings.push({ name, missingIn, zhOnly: [], enOnly: [] });
    continue;
  }

  const enKeys = new Set(flattenKeys(readNamespace('en', name)));
  const zhKeys = new Set(flattenKeys(readNamespace('zh', name)));
  const zhOnly = [...zhKeys].filter((key) => !enKeys.has(key)).sort();
  const enOnly = [...enKeys].filter((key) => !zhKeys.has(key)).sort();
  if (zhOnly.length > 0 || enOnly.length > 0) {
    findings.push({ name, missingIn: [], zhOnly, enOnly });
  }
}

if (findings.length > 0) {
  console.error('Locale consistency check failed:\n');
  for (const { name, missingIn, zhOnly, enOnly } of findings) {
    console.error(`  ${name}`);
    for (const locale of missingIn) {
      console.error(`    missing a "${locale}.json" counterpart`);
    }
    if (zhOnly.length > 0) {
      console.error('    present in zh but missing in en (English UI shows the raw key):');
      for (const key of zhOnly) {
        console.error(`      - ${key}`);
      }
    }
    if (enOnly.length > 0) {
      console.error('    present in en but missing in zh (Chinese UI falls back to English):');
      for (const key of enOnly) {
        console.error(`      - ${key}`);
      }
    }
  }
  console.error(`\nFix the namespaces above so locales/en and locales/zh stay in sync.`);
  process.exit(1);
}

console.log(`Locale consistency check passed: ${allNamespaces.length} namespaces in sync.`);

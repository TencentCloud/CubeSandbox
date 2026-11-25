// Locale consistency check.
//
// Two guarantees are enforced for every namespace JSON under src/locales:
//
//   1. every locale in LOCALES has a file for every namespace (and vice versa)
//   2. every locale exposes exactly the same set of flattened dotted keys as the
//      `en` baseline
//
// Key drift is reported in both directions, per locale:
//
//   extra key (in locale, absent from en) -> the English UI can only render the
//     raw key, so locales/en is the file that is missing data
//   missing key (in en, absent from locale) -> that UI silently falls back to en
//
// Exits 1 when any namespace is asymmetric so CI fails loudly; prints a clear
// success line and exits 0 otherwise. Dependency-free Node ESM.

import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const LOCALES = ['en', 'zh', 'zh-TW'];
// Reference locale that every other locale is compared against.
const BASELINE = 'en';
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
    findings.push({ name, missingIn, drift: [] });
    continue;
  }

  const baselineKeys = new Set(flattenKeys(readNamespace(BASELINE, name)));
  const drift = [];
  for (const locale of LOCALES.filter((candidate) => candidate !== BASELINE)) {
    const localeKeys = new Set(flattenKeys(readNamespace(locale, name)));
    const missing = [...baselineKeys].filter((key) => !localeKeys.has(key)).sort();
    const extra = [...localeKeys].filter((key) => !baselineKeys.has(key)).sort();
    if (missing.length > 0 || extra.length > 0) {
      drift.push({ locale, missing, extra });
    }
  }
  if (drift.length > 0) {
    findings.push({ name, missingIn: [], drift });
  }
}

if (findings.length > 0) {
  console.error('Locale consistency check failed:\n');
  for (const { name, missingIn, drift } of findings) {
    console.error(`  ${name}`);
    for (const locale of missingIn) {
      console.error(`    missing a "${locale}.json" counterpart`);
    }
    for (const { locale, missing, extra } of drift) {
      if (missing.length > 0) {
        console.error(
          `    in ${BASELINE} but missing in ${locale} (UI falls back to ${BASELINE}):`,
        );
        for (const key of missing) {
          console.error(`      - ${key}`);
        }
      }
      if (extra.length > 0) {
        console.error(`    in ${locale} but missing in ${BASELINE} (raw key rendered there):`);
        for (const key of extra) {
          console.error(`      - ${key}`);
        }
      }
    }
  }
  console.error(`\nFix the namespaces above so ${LOCALES.join(', ')} stay in sync.`);
  process.exit(1);
}

const compared = LOCALES.filter((locale) => locale !== BASELINE)
  .map((locale) => `${locale} vs ${BASELINE}`)
  .join(', ');
console.log(
  `Locale consistency check passed: ${allNamespaces.length} namespaces in sync (${compared}).`,
);

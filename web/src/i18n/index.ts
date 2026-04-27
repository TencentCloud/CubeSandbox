// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import i18n from 'i18next';
import LanguageDetector from 'i18next-browser-languagedetector';
import { initReactI18next } from 'react-i18next';
import { resources } from './resources';

i18n
  .use(LanguageDetector)
  .use(initReactI18next)
  .init({
    resources,
    fallbackLng: 'en',
    supportedLngs: ['en', 'zh'],
    defaultNS: 'common',
    ns: ['common', 'nav', 'topbar', 'command', 'overview', 'sandboxes', 'sandboxDetail', 'templates', 'nodes', 'keys', 'placeholder'],
    interpolation: {
      escapeValue: false,
    },
    detection: {
      order: ['localStorage', 'navigator'],
      caches: ['localStorage'],
      lookupLocalStorage: 'cube.lang',
      convertDetectedLanguage: (lng) => (lng.startsWith('zh') ? 'zh' : 'en'),
    },
  });

// Keep <html lang> in sync with the active language
i18n.on('languageChanged', (lng) => {
  document.documentElement.lang = lng;
});

// Set initial lang attribute
document.documentElement.lang = i18n.language;

export default i18n;

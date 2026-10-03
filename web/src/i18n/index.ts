// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import i18n from "i18next";
import { initReactI18next } from "react-i18next";

import aggregate from "./en/aggregate.json";
import audit from "./en/audit.json";
import auth from "./en/auth.json";
import common from "./en/common.json";
import dashboard from "./en/dashboard.json";
import env from "./en/env.json";
import envsettings from "./en/envsettings.json";
import fullpage from "./en/fullpage.json";
import install from "./en/install.json";
import overview from "./en/overview.json";
import reason from "./en/reason.json";
import settings from "./en/settings.json";
import setup from "./en/setup.json";
import shell from "./en/shell.json";
import switcher from "./en/switcher.json";

// English only for now (D-65). Resources are bundled statically, so there is
// no language detector and no backend plugin: nothing is fetched at runtime.
// Each later plan adds its own namespace file (setup, auth, shell, env,
// dashboard, audit, overview, install, reason, settings, envsettings, fullpage, switcher, aggregate) and registers it
// here, so parallel work never edits the same JSON. Keys are shaped
// area.screen.element. Plural keys use the i18next _one and _other suffixes.
export const resources = {
  en: {
    aggregate,
    audit,
    auth,
    common,
    dashboard,
    env,
    envsettings,
    fullpage,
    install,
    overview,
    reason,
    settings,
    setup,
    shell,
    switcher,
  },
} as const;

void i18n.use(initReactI18next).init({
  resources,
  lng: "en",
  fallbackLng: "en",
  defaultNS: "common",
  ns: Object.keys(resources.en),
  interpolation: {
    // React already escapes rendered values.
    escapeValue: false,
  },
});

export default i18n;

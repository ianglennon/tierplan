// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// The Settings tabs (D-60), in display order. Each is a route, so the address
// bar and the back button follow the tab. A tab shows only to a user who has the
// permission its page needs; the server checks it again. The tab bar and the
// /settings redirect both read this list, so they cannot disagree about which
// tab a user may open (WR-19). It lives here, not in a route file, so route
// modules export nothing but their Route.
export const SETTINGS_TABS = [
  { key: "general", to: "/settings/general", perm: "settings.read" },
  { key: "environments", to: "/settings/environments", perm: "environments.write" },
  { key: "about", to: "/settings/about", perm: "settings.read" },
] as const;

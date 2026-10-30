// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { hasAnyEnvironmentPermission, hasPermission, type Me } from "@/hooks/use-me";

// The Settings tabs (D-60), in display order. Each is a route, so the address
// bar and the back button follow the tab. A tab shows only to a user who has the
// permission its page needs; the server checks it again. The tab bar and the
// /settings redirect both read this list, so they cannot disagree about which
// tab a user may open (WR-19). It lives here, not in a route file, so route
// modules export nothing but their Route.
//
// scope says where the permission is held. "global" is an installation-wide
// permission. "anyEnvironment" is an environment permission, held on at least one
// environment (or on all of them): an env Admin has no installation-wide
// permission and still opens Environments, to administer their own (D-07).
export const SETTINGS_TABS = [
  { key: "general", to: "/settings/general", perm: "settings.read", scope: "global" },
  {
    key: "environments",
    to: "/settings/environments",
    perm: "environments.configure",
    scope: "anyEnvironment",
  },
  { key: "access", to: "/settings/access", perm: "users.manage", scope: "global" },
  { key: "backup", to: "/settings/backup", perm: "backup.manage", scope: "global" },
  { key: "about", to: "/settings/about", perm: "settings.read", scope: "global" },
] as const;

type SettingsTab = (typeof SETTINGS_TABS)[number];

// canOpenTab says whether the user holds what a tab's page needs.
export function canOpenTab(me: Me | undefined, tab: SettingsTab): boolean {
  return tab.scope === "anyEnvironment"
    ? hasAnyEnvironmentPermission(me, tab.perm)
    : hasPermission(me, tab.perm);
}

// visibleSettingsTabs is the tabs the user may open, in display order. Settings
// is in the sidebar exactly when this is not empty.
export function visibleSettingsTabs(me: Me | undefined): SettingsTab[] {
  return SETTINGS_TABS.filter((tab) => canOpenTab(me, tab));
}

// canManageEnvironments is true for someone who administers at least one
// environment, an instance admin included: it is what Manage environments in the
// switcher leads to. Like every check in the SPA it only decides what to show.
export function canManageEnvironments(me: Me | undefined): boolean {
  return hasAnyEnvironmentPermission(me, "environments.configure");
}

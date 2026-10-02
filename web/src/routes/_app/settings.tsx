// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute, Link, Outlet, useRouterState } from "@tanstack/react-router";

import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { useT } from "@/i18n/use-t";

// The Settings tabs (D-60). Each is a route, so the address bar and the back
// button follow the tab. Plan 26 adds General and About to this list.
const TABS = [{ key: "environments", to: "/settings/environments" }] as const;

// SettingsLayout is the Settings area: the page title and a tab bar driven by
// the URL. The Radix tab roles come from the Tabs components; the active tab is
// read from the location rather than held in state, and each trigger is a real
// link, so choosing a tab is a navigation and opens in a new tab like any link.
function SettingsLayout() {
  const t = useT();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const active = TABS.find((tab) => pathname === tab.to || pathname.startsWith(`${tab.to}/`))?.key;

  return (
    <div className="mx-auto flex w-full max-w-4xl flex-col gap-6">
      <h1 className="text-2xl font-semibold">{t("settings.title")}</h1>
      <Tabs value={active ?? ""} className="gap-6">
        <TabsList variant="line" aria-label={t("settings.tabsLabel")} className="h-9 max-md:h-11">
          {TABS.map((tab) => (
            <TabsTrigger
              key={tab.key}
              value={tab.key}
              asChild
              className="data-active:text-accent-ink after:bg-accent-ink max-md:h-11"
            >
              <Link
                to={tab.to}
                activeProps={{ "data-active": true }}
                inactiveProps={{ "data-active": undefined }}
              >
                {t(`settings.tabs.${tab.key}`)}
              </Link>
            </TabsTrigger>
          ))}
        </TabsList>
        <TabsContent value={active ?? ""} className="text-base">
          <Outlet />
        </TabsContent>
      </Tabs>
    </div>
  );
}

export const Route = createFileRoute("/_app/settings")({
  component: SettingsLayout,
});

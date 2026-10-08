// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute, Link, Outlet, useRouterState } from "@tanstack/react-router";

import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { SETTINGS_TABS, visibleSettingsTabs } from "@/features/settings/tabs";
import { useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// SettingsLayout is the Settings area: the page title and a tab bar driven by
// the URL. The Radix tab roles come from the Tabs components; the active tab is
// read from the location rather than held in state, and each trigger is a real
// link, so choosing a tab is a navigation and opens in a new tab like any link.
function SettingsLayout() {
  const t = useT();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const { data: me } = useMe();
  const tabs = visibleSettingsTabs(me);
  const active = SETTINGS_TABS.find(
    (tab) => pathname === tab.to || pathname.startsWith(`${tab.to}/`),
  )?.key;

  // An environment's own page holds a table of nodes, so it gets the wider
  // content area the UI-SPEC gives it (1280px); the other tabs stay narrow.
  const wide = pathname.startsWith("/settings/environments/");

  return (
    <div className={`mx-auto flex w-full flex-col gap-6 ${wide ? "max-w-7xl" : "max-w-4xl"}`}>
      <h1 className="text-2xl font-semibold">{t("settings.title")}</h1>
      <Tabs value={active ?? ""} className="gap-6">
        {/* Five tabs are wider than a phone. The bar scrolls inside this wrapper
            instead of widening the page, and the wrapper's bottom padding keeps
            the 2px active indicator, which hangs below the tabs, from being clipped. */}
        <div className="-mb-2 max-w-full overflow-x-auto pb-2">
          <TabsList variant="line" aria-label={t("settings.tabsLabel")} className="h-9 max-md:h-11">
            {tabs.map((tab) => (
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
        </div>
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

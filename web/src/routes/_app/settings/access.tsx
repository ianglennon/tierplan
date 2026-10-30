// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute, Link, Outlet, useRouterState } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// The sub-tabs of Settings > Access, in display order.
const ACCESS_TABS = [
  { key: "users", to: "/settings/access/users", label: "users.access.tabs.users" },
  { key: "teams", to: "/settings/access/teams", label: "teams.tabLabel" },
] as const;

// Settings > Access (D-25, D-60). Instance admins only: users.manage is global.
// The sub-tabs are URL-routed like the Settings tabs above them, so the address
// bar and the back button follow the tab, and each sub-tab sets its own document
// title. The server checks the permission again
// on every route.
function AccessTab() {
  const t = useT();
  const allowed = usePermission("users.manage");
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  if (!allowed) return <NoPermission />;
  const active = ACCESS_TABS.find(
    (tab) => pathname === tab.to || pathname.startsWith(`${tab.to}/`),
  )?.key;

  return (
    <div className="flex flex-col gap-6">
      <h2 className="text-xl font-semibold">{t("users.access.title")}</h2>
      <Tabs value={active ?? ""} className="gap-4">
        <TabsList
          variant="line"
          aria-label={t("users.access.tabsLabel")}
          className="h-9 max-md:h-11"
        >
          {ACCESS_TABS.map((tab) => (
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
                {t(tab.label)}
              </Link>
            </TabsTrigger>
          ))}
        </TabsList>
      </Tabs>
      <Outlet />
    </div>
  );
}

export const Route = createFileRoute("/_app/settings/access")({
  component: AccessTab,
});

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link, Outlet } from "@tanstack/react-router";

import { Tabs, TabsList, TabsScroller, TabsTrigger } from "@/components/ui/tabs";
import { IngressPageAlerts } from "@/features/ingress/page-alerts";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

export interface IngressTab {
  id: string;
  // The route the tab links to: the page's own address for Routes, a child of it for every other tab.
  to: "/env/$envId/ingress";
  // The i18n key of the tab's label.
  labelKey: string;
  // True when only the tab's own path is the tab, as for Routes, whose path starts every other tab's.
  exact: boolean;
}

// The tabs of the Ingress page in the UI-SPEC D-02 order. Later plans append Certificates, Nodes, Domains and
// Settings here, each a child route of the page.
export const INGRESS_TABS = [
  {
    id: "routes",
    to: "/env/$envId/ingress",
    labelKey: "ingressPage.tabs.routes",
    exact: true,
  },
] as const satisfies readonly IngressTab[];

// IngressLayout is the Ingress page's frame (UI-SPEC S1): the title, one line saying what the page is, and the tab
// bar of links, so each tab is linkable and Back goes tab to tab. The tab body renders in the Outlet.
export function IngressLayout({ envId }: { envId: string }) {
  const t = useT();
  useDocumentTitle(t("ingressPage.page.title"));
  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-col gap-1">
        <h2 className="text-2xl leading-tight font-semibold">{t("ingressPage.page.title")}</h2>
        <p className="text-sm text-muted-foreground">{t("ingressPage.page.description")}</p>
      </header>
      <Tabs value="" className="gap-4">
        <TabsScroller>
          <TabsList
            variant="line"
            aria-label={t("ingressPage.tabs.label")}
            className="h-9 max-md:h-11"
          >
            {INGRESS_TABS.map((tab) => (
              <TabsTrigger
                key={tab.id}
                value={tab.id}
                asChild
                className="data-active:text-accent-ink after:bg-accent-ink max-md:h-11"
              >
                <Link
                  to={tab.to}
                  params={{ envId }}
                  activeOptions={{ exact: tab.exact }}
                  activeProps={{ "data-active": true }}
                  inactiveProps={{ "data-active": undefined }}
                >
                  {t(tab.labelKey)}
                </Link>
              </TabsTrigger>
            ))}
          </TabsList>
        </TabsScroller>
      </Tabs>
      <IngressPageAlerts envId={envId} />
      <Outlet />
    </div>
  );
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link, useParams } from "@tanstack/react-router";
import { LayoutDashboard } from "lucide-react";

import { EnvSwitcher } from "@/components/env-switcher";
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  useSidebar,
} from "@/components/ui/sidebar";
import { VersionFooter } from "@/components/version-footer";
import { useT } from "@/i18n/use-t";

// AppSidebar is the left navigation (D-53): the environment switcher on top,
// the selected environment's sections, global items at the bottom, and the
// version footer. It collapses to a 48px icon rail and is a drawer under
// 768px.
//
// The version footer is in the sidebar only while the sidebar is expanded or
// its mobile drawer is open. Otherwise the app layout renders it at the bottom
// of the page, so exactly one is visible at every size and state (PLAT-05).
export function AppSidebar() {
  const t = useT();
  const { state, isMobile } = useSidebar();
  const footerHere = isMobile || state === "expanded";
  // The selected environment's sections show while a route has one.
  const { envId } = useParams({ strict: false });

  return (
    <Sidebar
      collapsible="icon"
      mobileTitle={t("shell.sidebar.title")}
      mobileDescription={t("shell.sidebar.description")}
    >
      <SidebarHeader className="h-12 justify-center border-b border-sidebar-border p-2">
        <EnvSwitcher />
      </SidebarHeader>
      {/* Plan 20 adds the environment's Resources group below Dashboard. */}
      <SidebarContent>
        {envId ? (
          <SidebarGroup>
            <SidebarGroupContent>
              <SidebarMenu>
                <SidebarMenuItem>
                  <SidebarMenuButton
                    asChild
                    tooltip={t("shell.nav.dashboard")}
                    className="max-md:h-11"
                  >
                    <Link
                      to="/env/$envId"
                      params={{ envId }}
                      activeOptions={{ exact: true }}
                      activeProps={{ "aria-current": "page", "data-active": true }}
                    >
                      <LayoutDashboard aria-hidden="true" />
                      <span>{t("shell.nav.dashboard")}</span>
                    </Link>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              </SidebarMenu>
            </SidebarGroupContent>
          </SidebarGroup>
        ) : null}
      </SidebarContent>
      {/* Settings and Audit are added by plans 23 and 25, once those routes exist
          for the typed links to point at. */}
      <SidebarGroup className="mt-auto p-0" />
      <SidebarFooter className="border-t border-sidebar-border px-4 py-2">
        {footerHere ? <VersionFooter /> : null}
      </SidebarFooter>
    </Sidebar>
  );
}

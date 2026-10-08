// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link, useParams } from "@tanstack/react-router";
import {
  Container,
  HardDrive,
  Layers,
  LayoutDashboard,
  LayoutGrid,
  Network,
  ScrollText,
  Settings,
} from "lucide-react";

import { EnvSwitcher } from "@/components/env-switcher";
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  useSidebar,
} from "@/components/ui/sidebar";
import { VersionFooter } from "@/components/version-footer";
import { visibleSettingsTabs } from "@/features/settings/tabs";
import { useAnyEnvironmentPermission, useCanReadAudit, useMe, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// AppSidebar is the left navigation (D-53): the environment switcher on top,
// the selected environment's sections, global items at the bottom, and the
// version footer. It collapses to a 48px icon rail and is a drawer under
// 768px.
//
// The version footer is in the sidebar only while the sidebar is expanded or
// its mobile drawer is open. Otherwise the app layout renders it at the bottom
// of the page, so exactly one is visible at every size and state (PLAT-05).
// The Resources group (D-56, UI-SPEC interpretation 2): the four read-only
// lists of the selected environment, each shown only to a user who may read it.
const RESOURCE_ITEMS = [
  { key: "containers", perm: "containers.read", to: "/env/$envId/containers", icon: Container },
  { key: "images", perm: "images.read", to: "/env/$envId/images", icon: Layers },
  { key: "volumes", perm: "volumes.read", to: "/env/$envId/volumes", icon: HardDrive },
  { key: "networks", perm: "networks.read", to: "/env/$envId/networks", icon: Network },
] as const;

function ResourceItem({ envId, item }: { envId: string; item: (typeof RESOURCE_ITEMS)[number] }) {
  const t = useT();
  const allowed = usePermission(item.perm, envId);
  if (!allowed) return null;
  const Icon = item.icon;
  const label = t(`shell.nav.${item.key}`);
  return (
    <SidebarMenuItem>
      <SidebarMenuButton asChild tooltip={label} className="max-md:h-11">
        <Link
          to={item.to}
          params={{ envId }}
          activeProps={{ "aria-current": "page", "data-active": true }}
        >
          <Icon aria-hidden="true" />
          <span>{label}</span>
        </Link>
      </SidebarMenuButton>
    </SidebarMenuItem>
  );
}

export function AppSidebar() {
  const t = useT();
  const { state, isMobile } = useSidebar();
  const footerHere = isMobile || state === "expanded";
  // The selected environment's sections show while a route has one.
  const { envId } = useParams({ strict: false });
  // Settings shows when any of its tabs does: an instance admin's pages and the
  // environment list, which an env Admin opens for their own environments alone
  // (D-53, D-07). The server checks each of them again.
  const { data: me } = useMe();
  const canSettings = visibleSettingsTabs(me).length > 0;
  // Audit shows to instance admins and to env Admins, who read only the entries of
  // the environments they administer (D-09, D-43). The server scopes the read.
  const canAudit = useCanReadAudit();
  // The overview is global. environments.read is held per environment, so it
  // shows for anyone who may read at least one; the server limits its rows.
  const canOverview = useAnyEnvironmentPermission("environments.read");

  return (
    <Sidebar
      collapsible="icon"
      mobileTitle={t("shell.sidebar.title")}
      mobileDescription={t("shell.sidebar.description")}
    >
      <SidebarHeader className="h-12 justify-center border-b border-sidebar-border p-2">
        <EnvSwitcher />
      </SidebarHeader>
      <SidebarContent>
        {canOverview ? (
          <SidebarGroup>
            <SidebarGroupContent>
              <SidebarMenu>
                <SidebarMenuItem>
                  <SidebarMenuButton
                    asChild
                    tooltip={t("shell.nav.environments")}
                    className="max-md:h-11"
                  >
                    <Link
                      to="/environments"
                      activeOptions={{ exact: true }}
                      activeProps={{ "aria-current": "page", "data-active": true }}
                    >
                      <LayoutGrid aria-hidden="true" />
                      <span>{t("shell.nav.environments")}</span>
                    </Link>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              </SidebarMenu>
            </SidebarGroupContent>
          </SidebarGroup>
        ) : null}
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
        {envId ? (
          <SidebarGroup>
            <SidebarGroupLabel>{t("shell.nav.resources")}</SidebarGroupLabel>
            <SidebarGroupContent>
              <SidebarMenu>
                {RESOURCE_ITEMS.map((item) => (
                  <ResourceItem key={item.key} envId={envId} item={item} />
                ))}
              </SidebarMenu>
            </SidebarGroupContent>
          </SidebarGroup>
        ) : null}
      </SidebarContent>
      <SidebarGroup className="mt-auto">
        <SidebarGroupContent>
          <SidebarMenu>
            {canAudit ? (
              <SidebarMenuItem>
                <SidebarMenuButton asChild tooltip={t("shell.nav.audit")} className="max-md:h-11">
                  <Link to="/audit" activeProps={{ "aria-current": "page", "data-active": true }}>
                    <ScrollText aria-hidden="true" />
                    <span>{t("shell.nav.audit")}</span>
                  </Link>
                </SidebarMenuButton>
              </SidebarMenuItem>
            ) : null}
            {canSettings ? (
              <SidebarMenuItem>
                <SidebarMenuButton
                  asChild
                  tooltip={t("shell.nav.settings")}
                  className="max-md:h-11"
                >
                  <Link
                    to="/settings"
                    activeProps={{ "aria-current": "page", "data-active": true }}
                  >
                    <Settings aria-hidden="true" />
                    <span>{t("shell.nav.settings")}</span>
                  </Link>
                </SidebarMenuButton>
              </SidebarMenuItem>
            ) : null}
          </SidebarMenu>
        </SidebarGroupContent>
      </SidebarGroup>
      <SidebarFooter className="border-t border-sidebar-border px-4 py-2">
        {footerHere ? <VersionFooter /> : null}
      </SidebarFooter>
    </Sidebar>
  );
}

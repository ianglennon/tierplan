// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link, useParams, useRouterState } from "@tanstack/react-router";
import {
  Activity,
  Boxes,
  Container,
  FileCog,
  Gauge,
  GitBranch,
  HardDrive,
  KeyRound,
  Layers,
  LayoutDashboard,
  LayoutGrid,
  LockKeyhole,
  Network,
  ScrollText,
  Server,
  Settings,
  SquareStack,
  X,
} from "lucide-react";
import { useEffect } from "react";
import { EnvSwitcher } from "@/components/env-switcher";
import { Button } from "@/components/ui/button";
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
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { visibleSettingsTabs } from "@/features/settings/tabs";
import {
  hasPermission,
  useAnyEnvironmentPermission,
  useCanReadAudit,
  useMe,
  usePermission,
} from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// AppSidebar is the left navigation (D-53): the environment switcher on top,
// the selected environment's sections, global items at the bottom, and the
// version footer. It collapses to a 48px icon rail and is a drawer under
// 768px.
//
// The version footer is in the sidebar only while the sidebar is expanded or
// its mobile drawer is open. Otherwise the app layout renders it at the bottom
// of the page, so exactly one is visible at every size and state (PLAT-05).
// The Resources group (D-56, UI-SPEC interpretation 2): the read-only
// lists of the selected environment and its event feed, each shown only to a user
// who may read it.
const RESOURCE_ITEMS = [
  // Stacks (Phase 7, UI-SPEC "Sidebar"): the first item, on swarm and standalone environments alike.
  { key: "stacks", perm: "stacks.read", to: "/env/$envId/stacks", icon: SquareStack },
  { key: "containers", perm: "containers.read", to: "/env/$envId/containers", icon: Container },
  { key: "images", perm: "images.read", to: "/env/$envId/images", icon: Layers },
  { key: "volumes", perm: "volumes.read", to: "/env/$envId/volumes", icon: HardDrive },
  { key: "networks", perm: "networks.read", to: "/env/$envId/networks", icon: Network },
  // Address space (Phase 10, D-01): below Networks, on swarm and standalone environments alike.
  { key: "addressSpace", perm: "networks.read", to: "/env/$envId/address-space", icon: Gauge },
  // The events stream already needs environments.read, so no new permission (D-51).
  { key: "events", perm: "environments.read", to: "/env/$envId/events", icon: Activity },
] as const;

// The Swarm group (Phase 6, UI-SPEC "Sidebar"): the swarm-only pages of the selected
// environment, below Resources. Shown on a swarm environment alone, each item to a user who may
// read it.
const SWARM_ITEMS = [
  { key: "services", perm: "services.read", to: "/env/$envId/services", icon: Boxes },
  { key: "nodes", perm: "nodes.read", to: "/env/$envId/nodes", icon: Server },
  { key: "placement", perm: "services.read", to: "/env/$envId/placement", icon: LayoutGrid },
  { key: "secrets", perm: "secrets.read", to: "/env/$envId/secrets", icon: LockKeyhole },
  { key: "configs", perm: "configs.read", to: "/env/$envId/configs", icon: FileCog },
] as const;

type NavItem = (typeof RESOURCE_ITEMS)[number] | (typeof SWARM_ITEMS)[number];

function ResourceItem({
  envId,
  item,
  onNavigate,
}: {
  envId: string;
  item: NavItem;
  onNavigate: () => void;
}) {
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
          onClick={onNavigate}
          activeProps={{ "aria-current": "page", "data-active": true }}
        >
          <Icon aria-hidden="true" />
          <span>{label}</span>
        </Link>
      </SidebarMenuButton>
    </SidebarMenuItem>
  );
}

function SwarmGroup({ envId, onNavigate }: { envId: string; onNavigate: () => void }) {
  const t = useT();
  const isSwarm = useEnvIsSwarm(envId);
  const { data: me } = useMe();
  // Hiding the group is a convenience, not a control: every services route checks
  // services.read on the server.
  const readable = SWARM_ITEMS.some((item) => hasPermission(me, item.perm, envId));
  if (!isSwarm || !readable) return null;
  return (
    <SidebarGroup>
      <SidebarGroupLabel>{t("shell.nav.swarm")}</SidebarGroupLabel>
      <SidebarGroupContent>
        <SidebarMenu>
          {SWARM_ITEMS.map((item) => (
            <ResourceItem key={item.key} envId={envId} item={item} onNavigate={onNavigate} />
          ))}
        </SidebarMenu>
      </SidebarGroupContent>
    </SidebarGroup>
  );
}

export function AppSidebar() {
  const t = useT();
  const { state, isMobile, setOpenMobile } = useSidebar();
  const footerHere = isMobile || state === "expanded";
  // The drawer closes when a page is chosen (G-05-7a): a link closes it on click, which
  // also covers tapping the page you are already on, and a change of path closes it
  // too, which covers the environment switcher and redirects.
  const closeMenu = () => {
    if (isMobile) setOpenMobile(false);
  };
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  // biome-ignore lint/correctness/useExhaustiveDependencies: it runs when the path changes, and only then
  useEffect(() => {
    setOpenMobile(false);
  }, [pathname]);
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
  // Registries is global too (D-09): the stored credentials, shown to anyone who may
  // read images on at least one environment. The server limits the rows it lists.
  const canRegistries = useAnyEnvironmentPermission("images.read");
  // All stacks is global too (D-21): the stacks of every environment the user may read them on. The
  // server limits the rows it lists.
  const canAllStacks = useAnyEnvironmentPermission("stacks.read");
  // Git is global too: the repositories and credentials that apply to the environments the user
  // may read them on. The server limits the rows it lists.
  const canGit = useAnyEnvironmentPermission("git.read");

  return (
    <Sidebar
      collapsible="icon"
      mobileTitle={t("shell.sidebar.title")}
      mobileDescription={t("shell.sidebar.description")}
    >
      <SidebarHeader className="h-12 justify-center border-b border-sidebar-border p-2 max-md:h-14 max-md:flex-row max-md:items-center">
        <div className="min-w-0 flex-1">
          <EnvSwitcher />
        </div>
        {isMobile ? (
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="size-11 shrink-0"
            aria-label={t("shell.sidebar.close")}
            onClick={() => setOpenMobile(false)}
          >
            <X aria-hidden="true" />
          </Button>
        ) : null}
      </SidebarHeader>
      <SidebarContent>
        {canOverview || canRegistries || canAllStacks || canGit ? (
          <SidebarGroup>
            <SidebarGroupContent>
              <SidebarMenu>
                {canOverview ? (
                  <SidebarMenuItem>
                    <SidebarMenuButton
                      asChild
                      tooltip={t("shell.nav.environments")}
                      className="max-md:h-11"
                    >
                      <Link
                        to="/environments"
                        onClick={closeMenu}
                        activeOptions={{ exact: true }}
                        activeProps={{ "aria-current": "page", "data-active": true }}
                      >
                        <LayoutGrid aria-hidden="true" />
                        <span>{t("shell.nav.environments")}</span>
                      </Link>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                ) : null}
                {canAllStacks ? (
                  <SidebarMenuItem>
                    <SidebarMenuButton
                      asChild
                      tooltip={t("shell.nav.allStacks")}
                      className="max-md:h-11"
                    >
                      <Link
                        to="/stacks"
                        onClick={closeMenu}
                        activeProps={{ "aria-current": "page", "data-active": true }}
                      >
                        <SquareStack aria-hidden="true" />
                        <span>{t("shell.nav.allStacks")}</span>
                      </Link>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                ) : null}
                {canRegistries ? (
                  <SidebarMenuItem>
                    <SidebarMenuButton
                      asChild
                      tooltip={t("shell.nav.registries")}
                      className="max-md:h-11"
                    >
                      <Link
                        to="/registries"
                        onClick={closeMenu}
                        activeProps={{ "aria-current": "page", "data-active": true }}
                      >
                        <KeyRound aria-hidden="true" />
                        <span>{t("shell.nav.registries")}</span>
                      </Link>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                ) : null}
                {canGit ? (
                  <SidebarMenuItem>
                    <SidebarMenuButton asChild tooltip={t("shell.nav.git")} className="max-md:h-11">
                      <Link
                        to="/git"
                        onClick={closeMenu}
                        activeProps={{ "aria-current": "page", "data-active": true }}
                      >
                        <GitBranch aria-hidden="true" />
                        <span>{t("shell.nav.git")}</span>
                      </Link>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                ) : null}
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
                      onClick={closeMenu}
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
                  <ResourceItem key={item.key} envId={envId} item={item} onNavigate={closeMenu} />
                ))}
              </SidebarMenu>
            </SidebarGroupContent>
          </SidebarGroup>
        ) : null}
        {envId ? <SwarmGroup envId={envId} onNavigate={closeMenu} /> : null}
      </SidebarContent>
      <SidebarGroup className="mt-auto">
        <SidebarGroupContent>
          <SidebarMenu>
            {canAudit ? (
              <SidebarMenuItem>
                <SidebarMenuButton asChild tooltip={t("shell.nav.audit")} className="max-md:h-11">
                  <Link
                    to="/audit"
                    onClick={closeMenu}
                    activeProps={{ "aria-current": "page", "data-active": true }}
                  >
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
                    onClick={closeMenu}
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

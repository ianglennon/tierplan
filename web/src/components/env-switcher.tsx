// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { Check, ChevronsUpDown, Server, Settings2 } from "lucide-react";
import { useEffect, useState } from "react";

import { environmentsQuery } from "@/api/environments";
import { StatusDot, useHealthLabel } from "@/components/status-dot";
import { TypeBadge } from "@/components/type-badge";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  useSidebar,
} from "@/components/ui/sidebar";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";

// EnvSwitcher is the sidebar header (D-54): always shown, even with one
// environment. The trigger holds the selected environment's name (cut with a
// tooltip when long), its type badge and its health dot; the menu lists every
// environment with the same three.
export function EnvSwitcher() {
  const t = useT();
  const healthLabel = useHealthLabel();
  const environments = useQuery(environmentsQuery);
  const { envId } = useParams({ strict: false });
  const { state, isMobile } = useSidebar();
  // Pages outside an environment (Settings, later) keep showing the one the
  // user was last on.
  const [lastId, setLastId] = useState<string | undefined>();
  useEffect(() => {
    if (envId) setLastId(envId);
  }, [envId]);

  // The list query keeps its last data when a refetch fails, so a failure
  // keeps the last known name and adds the inline notice below.
  const items = environments.data?.items ?? [];
  const selected =
    items.find((e) => e.id === envId) ?? items.find((e) => e.id === lastId) ?? items[0];

  return (
    <SidebarMenu>
      <SidebarMenuItem>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <SidebarMenuButton
              aria-label={t("shell.switcher.menu")}
              // The name is only worth a tooltip on the icon rail. A tooltip that
              // opens when the mobile drawer focuses this button would take the
              // first Escape from the drawer.
              tooltip={
                state === "collapsed" && !isMobile
                  ? (selected?.name ?? t("shell.switcher.none"))
                  : undefined
              }
              className="h-8 max-md:h-11"
            >
              <Server aria-hidden="true" />
              {selected ? (
                <>
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <span className="min-w-0 flex-1 truncate text-start">{selected.name}</span>
                    </TooltipTrigger>
                    <TooltipContent side="right">{selected.name}</TooltipContent>
                  </Tooltip>
                  <TypeBadge type={selected.type} />
                  <StatusDot state={selected.status.state} />
                </>
              ) : (
                <span className="min-w-0 flex-1 truncate text-start text-muted-foreground">
                  {t("shell.switcher.none")}
                </span>
              )}
              <ChevronsUpDown className="ms-auto shrink-0 opacity-60" aria-hidden="true" />
            </SidebarMenuButton>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start" className="min-w-64">
            <DropdownMenuLabel className="text-xs text-muted-foreground">
              {t("shell.switcher.label")}
            </DropdownMenuLabel>
            {items.map((env) => (
              <DropdownMenuItem key={env.id} asChild className="max-md:min-h-11">
                <Link to="/env/$envId" params={{ envId: env.id }}>
                  <span className="flex min-w-0 flex-1 flex-col gap-1">
                    <span className="flex min-w-0 items-center gap-2">
                      <span className="min-w-0 flex-1 truncate" title={env.name}>
                        {env.name}
                      </span>
                      <TypeBadge type={env.type} />
                    </span>
                    <StatusDot state={env.status.state} label={healthLabel(env.status)} />
                  </span>
                  {env.id === selected?.id ? (
                    <Check className="size-4 shrink-0" aria-hidden="true" />
                  ) : null}
                </Link>
              </DropdownMenuItem>
            ))}
            {environments.isError ? (
              <DropdownMenuItem disabled className="text-status-offline-text">
                {t("shell.switcher.loadFailed")}
              </DropdownMenuItem>
            ) : null}
            <DropdownMenuSeparator />
            {/* Plan 23 retargets this to /settings/environments. */}
            <DropdownMenuItem asChild className="max-md:min-h-11">
              <Link to="/">
                <Settings2 aria-hidden="true" />
                {t("shell.switcher.manage")}
              </Link>
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </SidebarMenuItem>
    </SidebarMenu>
  );
}

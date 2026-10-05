// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link, useLocation, useNavigate, useParams } from "@tanstack/react-router";
import { Check, ChevronsUpDown, LayoutGrid, Server, Settings2 } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { type EnvironmentItem, environmentsQuery, recentQuery } from "@/api/environments";
import { StatusDot } from "@/components/status-dot";
import { TypeBadge } from "@/components/type-badge";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { Kbd } from "@/components/ui/kbd";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  useSidebar,
} from "@/components/ui/sidebar";
import { Skeleton } from "@/components/ui/skeleton";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { isMacPlatform, shortcutLabel, switchTarget } from "@/lib/env-nav";
import { cn } from "@/lib/utils";

// byName is the order of the Environments group: alphabetical, ignoring case
// and accents, with the ID breaking a tie so the order never flickers.
function byName(a: EnvironmentItem, b: EnvironmentItem): number {
  return (
    a.name.localeCompare(b.name, undefined, { sensitivity: "base" }) || a.id.localeCompare(b.id)
  );
}

// useEnvChoices is what both lists (the sidebar's and the Cmd/Ctrl+K palette)
// show: every environment alphabetically, the user's last few from the server,
// and whether the list is still loading for the first time or failed to load.
// The query keeps its last data when a reload fails, and the global health
// stream keeps the dots current, so neither list opens a stream of its own.
export function useEnvChoices() {
  const environments = useQuery(environmentsQuery);
  const recent = useQuery(recentQuery);
  const items = [...(environments.data?.items ?? [])].sort(byName);
  // The server already leaves out environments that are gone, but the two
  // reads can disagree for a moment, so a recent ID with no environment is
  // dropped here too.
  const recentItems = (recent.data ?? []).flatMap((id) => {
    const found = items.find((e) => e.id === id);
    return found ? [found] : [];
  });
  return {
    items,
    recentItems,
    loading: environments.isPending,
    failed: environments.isError,
  };
}

// useSelectedEnvironment is the environment the user is "in": the one in the
// address, else the last one they were in (Settings and the overview keep
// showing it), else the first alphabetically. Both lists mark it with a check.
export function useSelectedEnvironment(items: EnvironmentItem[]): EnvironmentItem | undefined {
  const { envId } = useParams({ strict: false });
  const [lastId, setLastId] = useState<string | undefined>();
  useEffect(() => {
    if (envId) setLastId(envId);
  }, [envId]);
  return items.find((e) => e.id === envId) ?? items.find((e) => e.id === lastId) ?? items[0];
}

// choiceFilter is the list's filter: the text typed must be inside the name or
// the type ("swarm", "standalone", "local socket"). cmdk's own filter is fuzzy
// and also reads the value, which here is an ID, so it is replaced.
export function choiceFilter(_value: string, search: string, keywords?: string[]): number {
  const q = search.trim().toLowerCase();
  if (q === "") return 1;
  return (keywords ?? []).some((k) => k.toLowerCase().includes(q)) ? 1 : 0;
}

// useChoose is how an item takes the user somewhere. Every item is a link, so a
// pointer click is the link's own (and a Ctrl or middle click opens a tab);
// cmdk also calls onSelect for that click, where only the list needs closing.
// Enter reaches onSelect alone, and that is the one that navigates.
function useChoose(onChosen: () => void) {
  const navigate = useNavigate();
  const clicked = useRef(false);
  return {
    onLinkClick: () => {
      clicked.current = true;
    },
    select: (to: string) => {
      if (clicked.current) clicked.current = false;
      else void navigate({ to });
      onChosen();
    },
  };
}

// CompactTypeBadge is the one badge the switcher has room for. The trigger and
// a list row are narrower than a name and two badges, and a name that is cut to
// "this-h…" is worse than a missing "Standalone". The local socket is the more
// telling of the two, and the full pair is on the overview and the dashboard.
function CompactTypeBadge({ env }: { env: EnvironmentItem }) {
  return (
    <TypeBadge
      envType={env.isLocalSocket ? undefined : env.envType}
      type={env.isLocalSocket ? undefined : env.type}
      isLocalSocket={env.isLocalSocket}
    />
  );
}

interface EnvItemProps {
  env: EnvironmentItem;
  group: "recent" | "env";
  selected: boolean;
  to: string;
  onLinkClick: () => void;
  select: (to: string) => void;
}

// EnvChoice is one environment in a list: the name (cut with a title when
// long), its type badge and a dot with no text, which carries the health as its
// name so it is never colour alone. It is a link, so open-in-new-tab works.
function EnvChoice({ env, group, selected, to, onLinkClick, select }: EnvItemProps) {
  const t = useT();
  // The same reading of the type as TypeBadge: nothing until it is known, so
  // a pending environment is not found by a type it does not show.
  const kind =
    env.envType ?? (env.type ? (env.type === "standalone" ? "standalone" : "swarm") : undefined);
  return (
    <CommandItem
      asChild
      value={`${group}:${env.id}`}
      keywords={[
        env.name,
        ...(kind ? [t(`env.type.${kind}`)] : []),
        ...(env.isLocalSocket ? [t("env.type.localSocket")] : []),
      ]}
      data-checked={selected ? "true" : undefined}
      onSelect={() => select(to)}
      className="max-md:min-h-11"
    >
      <Link to={to} aria-current={selected ? "page" : undefined} onClick={onLinkClick}>
        <span className="min-w-0 flex-1 truncate" title={env.name}>
          {env.name}
        </span>
        <CompactTypeBadge env={env} />
        <StatusDot state={env.status.state} />
        {selected ? <Check className="size-4" aria-hidden="true" /> : null}
      </Link>
    </CommandItem>
  );
}

interface EnvCommandItemsProps {
  // The text in the filter. While there is some, the Recent group is left out:
  // the same environment twice in a short list of matches only gets in the way.
  query: string;
  // The environment shown as the current one (it gets the check).
  selectedId: string | undefined;
  // Where the user is, for the keep-the-section rule.
  pathname: string;
  // Closes whatever holds the list.
  onChosen: () => void;
  // Manage environments is for settings holders and belongs to the sidebar.
  showManage?: boolean;
}

// EnvCommandItems is the body of both lists (D-35): Recent, Environments, the
// empty and failed notices, and the All environments item that is always there.
// It is a set of cmdk children, so the sidebar's popover and the palette's
// dialog each wrap it in their own Command.
export function EnvCommandItems({
  query,
  selectedId,
  pathname,
  onChosen,
  showManage = false,
}: EnvCommandItemsProps) {
  const t = useT();
  const { items, recentItems, loading, failed } = useEnvChoices();
  const { onLinkClick, select } = useChoose(onChosen);
  const choice = (env: EnvironmentItem, group: "recent" | "env") => (
    <EnvChoice
      key={`${group}:${env.id}`}
      env={env}
      group={group}
      selected={env.id === selectedId}
      to={switchTarget(pathname, env.id)}
      onLinkClick={onLinkClick}
      select={select}
    />
  );

  return (
    <CommandList className="max-h-80" aria-busy={loading || undefined}>
      {loading ? (
        <div className="p-1" role="status" aria-label={t("switcher.loading")}>
          <Skeleton className="h-8 w-full max-md:h-11" />
        </div>
      ) : null}
      {query === "" && recentItems.length > 0 ? (
        <CommandGroup heading={t("switcher.recent")}>
          {recentItems.map((env) => choice(env, "recent"))}
        </CommandGroup>
      ) : null}
      {items.length > 0 ? (
        <CommandGroup heading={t("switcher.environments")}>
          {items.map((env) => choice(env, "env"))}
        </CommandGroup>
      ) : null}
      {failed ? (
        <CommandGroup forceMount>
          <CommandItem
            disabled
            forceMount
            value="load-failed"
            className="text-status-offline-text max-md:min-h-11"
          >
            {t("switcher.loadFailed")}
          </CommandItem>
        </CommandGroup>
      ) : null}
      {query !== "" && !loading ? (
        <CommandEmpty>{t("switcher.noMatch", { query })}</CommandEmpty>
      ) : null}
      {/* Pinned to the bottom of the scrolling list, so All environments is
          there however many environments there are. */}
      <div className="sticky bottom-0 z-10 border-t bg-popover">
        <CommandGroup forceMount>
          <CommandItem
            asChild
            forceMount
            value="all-environments"
            onSelect={() => select("/environments")}
            className="max-md:min-h-11"
          >
            <Link to="/environments" onClick={onLinkClick}>
              <LayoutGrid aria-hidden="true" />
              {t("switcher.all")}
            </Link>
          </CommandItem>
          {showManage ? (
            <CommandItem
              asChild
              forceMount
              value="manage-environments"
              onSelect={() => select("/settings/environments")}
              className="max-md:min-h-11"
            >
              <Link to="/settings/environments" onClick={onLinkClick}>
                <Settings2 aria-hidden="true" />
                {t("switcher.manage")}
              </Link>
            </CommandItem>
          ) : null}
        </CommandGroup>
      </div>
    </CommandList>
  );
}

// EnvSwitcher is the sidebar header (D-54): always shown, even with one
// environment. The trigger holds the selected environment's name (cut with a
// tooltip when long), its type badge and its health dot; it opens a list with a
// filter, the user's recent environments, all of them, and a footer (D-35).
export function EnvSwitcher() {
  const t = useT();
  const { items, loading } = useEnvChoices();
  const pathname = useLocation({ select: (l) => l.pathname });
  const { state, isMobile } = useSidebar();
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  // Manage environments leads to Settings, which is for these two (the sidebar
  // uses the same test for its Settings item). The server checks each page.
  const canReadSettings = usePermission("settings.read");
  const canManageEnvironments = usePermission("environments.manage");
  const canSettings = canReadSettings || canManageEnvironments;

  // The list query keeps its last data when a refetch fails, so a failure keeps
  // the last known name and the list adds the notice.
  const selected = useSelectedEnvironment(items);

  const changeOpen = (next: boolean) => {
    setOpen(next);
    if (!next) setQuery("");
  };

  return (
    <SidebarMenu>
      <SidebarMenuItem>
        <Popover open={open} onOpenChange={changeOpen}>
          <PopoverTrigger asChild>
            <SidebarMenuButton
              aria-label={t("shell.switcher.menu")}
              aria-keyshortcuts={isMacPlatform() ? "Meta+K" : "Control+K"}
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
                  {open ? (
                    // A tooltip over the open list would sit on top of its badge.
                    <span className="min-w-12 flex-1 truncate text-start">{selected.name}</span>
                  ) : (
                    <Tooltip>
                      <TooltipTrigger asChild>
                        <span className="min-w-12 flex-1 truncate text-start">{selected.name}</span>
                      </TooltipTrigger>
                      <TooltipContent side="right">{selected.name}</TooltipContent>
                    </Tooltip>
                  )}
                  <CompactTypeBadge env={selected} />
                  <StatusDot state={selected.status.state} />
                </>
              ) : (
                <span className="min-w-0 flex-1 truncate text-start text-muted-foreground">
                  {loading ? "" : t("shell.switcher.none")}
                </span>
              )}
              <ChevronsUpDown className="ms-auto shrink-0 opacity-60" aria-hidden="true" />
            </SidebarMenuButton>
          </PopoverTrigger>
          <PopoverContent
            align="start"
            className="w-(--radix-popover-trigger-width) min-w-72 gap-0 p-0"
          >
            <Command filter={choiceFilter} label={t("switcher.filter")} loop vimBindings={false}>
              <CommandInput
                autoFocus
                value={query}
                onValueChange={setQuery}
                placeholder={t("switcher.filter")}
              />
              <EnvCommandItems
                query={query}
                selectedId={selected?.id}
                pathname={pathname}
                onChosen={() => changeOpen(false)}
                showManage={canSettings}
              />
              <div
                className={cn(
                  "flex items-center justify-between gap-2 border-t px-2 py-1.5",
                  "text-xs text-muted-foreground",
                )}
              >
                <span>{t("switcher.shortcut")}</span>
                <Kbd>{shortcutLabel()}</Kbd>
              </div>
            </Command>
          </PopoverContent>
        </Popover>
      </SidebarMenuItem>
    </SidebarMenu>
  );
}

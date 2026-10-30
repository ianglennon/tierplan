// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Link } from "@tanstack/react-router";
import { ChevronDown, ChevronsUpDown, ChevronUp } from "lucide-react";
import { type FocusEvent, useEffect, useMemo, useRef, useState } from "react";

import type { EnvironmentItem, EnvironmentStatus } from "@/api/environments";
import { RelativeTime } from "@/components/relative-time";
import { StatusDot, useHealthLabel } from "@/components/status-dot";
import { TypeBadge } from "@/components/type-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Item, ItemGroup } from "@/components/ui/item";
import { Skeleton } from "@/components/ui/skeleton";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { isKnownReason, ReasonText } from "@/features/environments/reason-text";
import { type SortDir, type SortKey, sortEnvironments } from "@/features/environments/sort";
import { useIsMobile } from "@/hooks/use-mobile";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

export interface EnvOverviewTableProps {
  items: EnvironmentItem[];
  // True while the first list is loading: six skeleton rows keep the layout still.
  loading?: boolean;
  // The sort from the URL. With none, the order is problems first.
  sort?: SortKey;
  dir?: SortDir;
  onSortChange: (sort: SortKey, dir: SortDir) => void;
  // Whether this caller may finish setting up a pending environment
  // (environments.token on it). The button is left out without it.
  canFinishSetup?: (envId: string) => boolean;
  // The button is handed over so the dialog it opens can give focus back to it.
  onFinishSetup?: (envId: string, trigger: HTMLElement) => void;
}

// One row is 36px and one header row is 48px; both are 44px under 768px for
// touch (UI-SPEC Spacing).
const ROW = "h-9 max-md:h-11";
const SKELETON_ROWS = 6;
// How long a changed row shows the muted background.
const FLASH_MS = 150;
// A screen reader hears at most one announcement in this time.
const ANNOUNCE_MS = 5_000;

const SORTABLE: ReadonlySet<string> = new Set<SortKey>([
  "health",
  "name",
  "nodes",
  "failing",
  "lastSeen",
]);

const COLUMNS = [
  "health",
  "name",
  "type",
  "nodes",
  "failing",
  "agents",
  "engine",
  "agent",
  "lastSeen",
] as const;

// What an overview row says about a state that is not online: ReasonText's
// words for the code the server gave, or when it has none, or one this build
// does not know, the wording for the state itself. Nothing for a state that is
// fine or has not been probed. The server sends a code, not wording, and every
// string comes from our own tables, so nothing an agent supplied is markup
// (T-02-50, T-02-61).
function OverviewReason({
  status,
  envType,
  variant,
}: {
  status: EnvironmentStatus;
  envType: EnvironmentItem["envType"];
  variant: "tooltip" | "compact";
}) {
  const t = useT();
  const { state, reason } = status;
  if (state === "online" || state === "unknown") return null;
  // Pending and reconnecting say what they are, whatever else is attached.
  const code = state !== "pending" && state !== "reconnecting" ? reason : undefined;
  if (isKnownReason(code)) {
    return (
      <ReasonText
        code={code}
        variant={variant}
        params={{ envType }}
        className={variant === "compact" ? "text-xs" : undefined}
      />
    );
  }
  const base = `overview.reason.states.${state}`;
  return (
    <div className="flex flex-col gap-0.5 text-xs">
      <p>{t(`${base}.text`)}</p>
      <p className={variant === "tooltip" ? "opacity-80" : "text-muted-foreground"}>
        {t(`${base}.hint`)}
      </p>
    </div>
  );
}

// Whether a status has anything to explain.
function hasReason(status: EnvironmentStatus): boolean {
  return status.state !== "online" && status.state !== "unknown";
}

// What makes a row worth a flash: anything it shows, but not the last-seen
// time, which moves on every heartbeat.
function signature(e: EnvironmentItem): string {
  return JSON.stringify([
    e.name,
    e.status,
    e.envType,
    e.nodes,
    e.agents,
    e.failingServices,
    e.agentVersion,
    e.updateAvailable,
    e.engineVersion,
  ]);
}

function prefersReducedMotion(): boolean {
  return (
    typeof window.matchMedia === "function" &&
    window.matchMedia("(prefers-reduced-motion: reduce)").matches
  );
}

// useFlashing is the ids of the rows that changed in the last 150 ms. The first
// list never flashes, and nothing flashes under prefers-reduced-motion.
function useFlashing(items: EnvironmentItem[]): ReadonlySet<string> {
  const previous = useRef<Map<string, string> | null>(null);
  const timers = useRef(new Set<ReturnType<typeof setTimeout>>());
  const [flashing, setFlashing] = useState<ReadonlySet<string>>(new Set());

  useEffect(() => {
    const next = new Map(items.map((e) => [e.id, signature(e)]));
    const before = previous.current;
    previous.current = next;
    if (!before || prefersReducedMotion()) return;
    const changed = items.filter((e) => {
      const was = before.get(e.id);
      return was !== undefined && was !== next.get(e.id);
    });
    if (changed.length === 0) return;
    const ids = changed.map((e) => e.id);
    setFlashing((old) => new Set([...old, ...ids]));
    const timer = setTimeout(() => {
      timers.current.delete(timer);
      setFlashing((old) => {
        const rest = new Set(old);
        for (const id of ids) rest.delete(id);
        return rest;
      });
    }, FLASH_MS);
    timers.current.add(timer);
  }, [items]);

  useEffect(() => {
    const held = timers.current;
    return () => {
      for (const timer of held) clearTimeout(timer);
    };
  }, []);

  return flashing;
}

// useAnnouncer is the text of the polite live region: "{name} is now {state}".
// A change that lands within five seconds of the last announcement waits for
// the window to end, and the changes that waited are read together. The first
// list it is given announces nothing.
function useAnnouncer(items: EnvironmentItem[]): string {
  const t = useT();
  const previous = useRef<Map<string, string> | null>(null);
  const queue = useRef<string[]>([]);
  const lastAt = useRef(Number.NEGATIVE_INFINITY);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const [message, setMessage] = useState("");
  // The translator changes every render; the effects read the latest one.
  const translate = useRef(t);
  translate.current = t;

  useEffect(() => {
    function flush(): void {
      if (queue.current.length === 0 || timer.current !== undefined) return;
      const wait = lastAt.current + ANNOUNCE_MS - Date.now();
      if (wait > 0) {
        timer.current = setTimeout(() => {
          timer.current = undefined;
          flush();
        }, wait);
        return;
      }
      lastAt.current = Date.now();
      setMessage(queue.current.splice(0).join(". "));
    }
    const next = new Map(items.map((e) => [e.id, e.status.state as string]));
    const before = previous.current;
    previous.current = next;
    if (before) {
      for (const e of items) {
        const was = before.get(e.id);
        if (was === undefined || was === e.status.state) continue;
        queue.current.push(
          translate.current("overview.announce", {
            name: e.name,
            state: translate.current(`env.health.${e.status.state}`),
          }),
        );
      }
      flush();
    }
  }, [items]);

  useEffect(
    () => () => {
      clearTimeout(timer.current);
    },
    [],
  );

  return message;
}

// useRows is the rows in the order to show. While the pointer or focus is
// inside the table the order is held, so a row never jumps away under a click:
// rows still take their new content, and the new order is applied when the
// pointer and focus have both left. Changing the sort applies at once.
function useRows(
  items: EnvironmentItem[],
  sort: SortKey | undefined,
  dir: SortDir | undefined,
  held: boolean,
): EnvironmentItem[] {
  const order = useRef<{ key: string; ids: string[] }>({ key: "", ids: [] });
  return useMemo(() => {
    const sorted = sortEnvironments(items, sort, dir);
    const key = `${sort ?? ""}/${dir ?? ""}`;
    if (!held || order.current.key !== key) {
      order.current = { key, ids: sorted.map((e) => e.id) };
      return sorted;
    }
    const byId = new Map(items.map((e) => [e.id, e]));
    const kept = order.current.ids.filter((id) => byId.has(id));
    const known = new Set(kept);
    // An environment that appeared while the order was held goes at the end.
    const ids = [...kept, ...sorted.filter((e) => !known.has(e.id)).map((e) => e.id)];
    order.current = { key, ids };
    return ids.flatMap((id) => byId.get(id) ?? []);
  }, [items, sort, dir, held]);
}

function Dash() {
  const t = useT();
  return <span className="text-muted-foreground">{t("overview.cell.missing")}</span>;
}

// HealthCell is the dot with its label. Any state but online is focusable and
// carries the reason and a fix hint in a tooltip (UI-SPEC); the same wording is
// on the stacked row and on the environment's own page, so the tooltip is never
// the only place it lives.
function HealthCell({ env }: { env: EnvironmentItem }) {
  const healthLabel = useHealthLabel();
  const dot = <StatusDot state={env.status.state} label={healthLabel(env.status)} />;
  if (!hasReason(env.status)) return dot;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          // biome-ignore lint/a11y/noNoninteractiveTabindex: the reason tooltip must open on keyboard focus (UI-SPEC Environments overview)
          tabIndex={0}
          className="inline-flex rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        >
          {dot}
        </span>
      </TooltipTrigger>
      <TooltipContent className="max-w-72">
        <OverviewReason status={env.status} envType={env.envType} variant="tooltip" />
      </TooltipContent>
    </Tooltip>
  );
}

interface NameCellProps {
  env: EnvironmentItem;
  finish: boolean;
  onFinishSetup?: (envId: string, trigger: HTMLElement) => void;
  // Stretches the link over the whole stacked row, so the 44px row is the tap
  // target and not just the line of text. The row must be `relative`.
  stretch?: boolean;
}

// The name links to the environment. It truncates with an ellipsis and keeps
// the whole name in its title; the badge and the dot never truncate.
function NameCell({ env, finish, onFinishSetup, stretch }: NameCellProps) {
  const t = useT();
  return (
    <div className="flex min-w-0 items-center gap-2">
      <Link
        to="/env/$envId"
        params={{ envId: env.id }}
        title={env.name}
        className={cn(
          "block max-w-48 min-w-0 truncate rounded-sm font-semibold hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:max-w-none",
          stretch && "after:absolute after:inset-0 after:content-['']",
        )}
      >
        {env.name}
      </Link>
      {finish ? (
        <Button
          variant="link"
          size="xs"
          className="relative z-10 h-auto shrink-0 p-0"
          onClick={(event) => onFinishSetup?.(env.id, event.currentTarget)}
        >
          {t("overview.finishSetup")}
        </Button>
      ) : null}
    </div>
  );
}

// Count cells read "3/4" and say "3 of 4 nodes ready" to a screen reader. A
// zero is shown as 0; only a count that is not known is a dash.
function CountCell({ shown, spoken }: { shown: string; spoken: string }) {
  return (
    <span role="img" aria-label={spoken} className="tabular-nums">
      {shown}
    </span>
  );
}

function NodesCell({ env }: { env: EnvironmentItem }) {
  const t = useT();
  if (!env.nodes) return <Dash />;
  const { ready, total } = env.nodes;
  return (
    <CountCell
      shown={`${ready}/${total}`}
      spoken={t("overview.cell.nodesLabel", { count: total, ready, total })}
    />
  );
}

function FailingCell({ env }: { env: EnvironmentItem }) {
  const t = useT();
  if (!env.failingServices) return <Dash />;
  const { failing, total } = env.failingServices;
  return (
    <CountCell
      shown={`${failing}/${total}`}
      spoken={t("overview.cell.failingLabel", { count: total, failing, total })}
    />
  );
}

function AgentsCell({ env }: { env: EnvironmentItem }) {
  const t = useT();
  if (!env.agents) return <Dash />;
  const { connected, expected } = env.agents;
  return (
    <CountCell
      shown={`${connected}/${expected}`}
      spoken={t("overview.cell.agentsLabel", { count: expected, connected, expected })}
    />
  );
}

function AgentCell({ env }: { env: EnvironmentItem }) {
  const t = useT();
  if (!env.agentVersion) return <Dash />;
  return (
    <span className="inline-flex items-center gap-2">
      <span className="tabular-nums">{env.agentVersion}</span>
      {env.updateAvailable ? (
        <Badge variant="secondary" aria-label={t("overview.cell.updateLabel")}>
          {t("overview.cell.update")}
        </Badge>
      ) : null}
    </span>
  );
}

function LastSeenCell({ env }: { env: EnvironmentItem }) {
  const t = useT();
  if (env.status.state === "online") return <span>{t("overview.cell.now")}</span>;
  return <RelativeTime value={env.lastSeen ?? undefined} />;
}

interface HeaderProps {
  sort?: SortKey;
  dir?: SortDir;
  onSortChange: (sort: SortKey, dir: SortDir) => void;
}

function Header({ sort, dir, onSortChange }: HeaderProps) {
  const t = useT();
  // With no sort in the URL the table is in problems-first order, which is
  // the Health column ascending, so that is the one marked.
  const active = sort ?? "health";
  const direction = dir ?? "asc";
  return (
    <TableHeader>
      <TableRow className={cn("h-12 max-md:h-12", "hover:bg-transparent")}>
        {COLUMNS.map((column) => {
          const label = t(`overview.columns.${column}`);
          const sortable = SORTABLE.has(column);
          const here = sortable && column === active;
          return (
            <TableHead
              key={column}
              aria-sort={
                sortable
                  ? here
                    ? direction === "asc"
                      ? "ascending"
                      : "descending"
                    : "none"
                  : undefined
              }
              className={cn(
                "sticky top-0 z-10 h-12 bg-muted px-2 text-xs font-semibold text-muted-foreground",
                // The longest header may take two lines, so the table fits beside
                // the open sidebar at 1280px without scrolling sideways.
                column === "failing" && "whitespace-normal",
              )}
            >
              {sortable ? (
                <button
                  type="button"
                  aria-label={t("overview.sort.by", { column: label })}
                  onClick={() =>
                    onSortChange(column as SortKey, here && direction === "asc" ? "desc" : "asc")
                  }
                  className="inline-flex items-center gap-1 rounded-sm font-semibold hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11"
                >
                  {label}
                  {here ? (
                    direction === "asc" ? (
                      <ChevronUp className="size-3.5" aria-hidden="true" />
                    ) : (
                      <ChevronDown className="size-3.5" aria-hidden="true" />
                    )
                  ) : (
                    <ChevronsUpDown className="size-3.5 opacity-60" aria-hidden="true" />
                  )}
                </button>
              ) : (
                label
              )}
            </TableHead>
          );
        })}
      </TableRow>
    </TableHeader>
  );
}

// The second line of a stacked row: "3/3 nodes · 1 failing · agent 0.2.0".
function useSummaryLine(): (env: EnvironmentItem) => string {
  const t = useT();
  return (env) => {
    const parts: string[] = [];
    if (env.nodes) {
      parts.push(
        t("overview.mobile.nodes", {
          count: env.nodes.total,
          ready: env.nodes.ready,
          total: env.nodes.total,
        }),
      );
    }
    if (env.failingServices) {
      parts.push(t("overview.mobile.failing", { failing: env.failingServices.failing }));
    }
    if (env.agentVersion) parts.push(t("overview.mobile.agent", { version: env.agentVersion }));
    return parts.join(" · ");
  };
}

interface StackedRowProps extends Omit<NameCellProps, "finish"> {
  flash: boolean;
  finish: boolean;
}

// StackedRow is one environment under 768px: name, type and health on line 1,
// the counts on line 2, and the reason on line 3 for anything that is not
// online, since touch has no hover for the tooltip.
function StackedRow({ env, finish, onFinishSetup, flash }: StackedRowProps) {
  const healthLabel = useHealthLabel();
  const summary = useSummaryLine()(env);
  return (
    <Item
      role="listitem"
      variant="outline"
      data-flash={flash || undefined}
      className={cn(
        "relative min-h-11 flex-col flex-nowrap items-stretch gap-1 transition-colors duration-150",
        flash && "bg-muted",
      )}
    >
      {/* The badges and the health label wrap to a second line before they can
          squeeze the name to nothing. */}
      <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
        <div className="min-w-0 flex-1 basis-28">
          <NameCell env={env} finish={finish} onFinishSetup={onFinishSetup} stretch />
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <TypeBadge envType={env.envType} type={env.type} isLocalSocket={env.isLocalSocket} />
          <StatusDot state={env.status.state} label={healthLabel(env.status)} />
        </div>
      </div>
      {summary ? <p className="text-xs text-muted-foreground">{summary}</p> : null}
      {hasReason(env.status) ? (
        <OverviewReason status={env.status} envType={env.envType} variant="compact" />
      ) : null}
    </Item>
  );
}

// EnvOverviewTable is the overview of every environment (D-32): dense and
// sortable, problems first, updating in place from the one health stream. It
// holds the row order still while the pointer or focus is inside, flashes a
// changed row for 150 ms and keeps a polite live region for screen readers.
// Under 768px it is a stack of rows. It has no filter, no search and no
// pagination: it renders every environment.
export function EnvOverviewTable({
  items,
  loading,
  sort,
  dir,
  onSortChange,
  canFinishSetup,
  onFinishSetup,
}: EnvOverviewTableProps) {
  const t = useT();
  const mobile = useIsMobile();
  const [hovered, setHovered] = useState(false);
  const [focused, setFocused] = useState(false);
  const rows = useRows(items, sort, dir, hovered || focused);
  const flashing = useFlashing(items);
  const announcement = useAnnouncer(items);

  const region = {
    onPointerEnter: () => setHovered(true),
    onPointerLeave: () => setHovered(false),
    onFocus: () => setFocused(true),
    onBlur: (event: FocusEvent<HTMLElement>) => {
      if (!event.currentTarget.contains(event.relatedTarget)) setFocused(false);
    },
  };
  const finish = (env: EnvironmentItem) =>
    env.status.state === "pending" && Boolean(onFinishSetup) && Boolean(canFinishSetup?.(env.id));

  return (
    <>
      <output aria-live="polite" className="sr-only">
        {announcement}
      </output>
      {mobile ? (
        <ItemGroup aria-busy={loading || undefined} className="gap-2" {...region}>
          {loading
            ? Array.from({ length: SKELETON_ROWS }, (_, i) => (
                // Placeholder rows have no identity of their own.
                // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows
                <Item key={i} role="listitem" variant="outline" className="min-h-11">
                  <Skeleton className="h-4 w-40" aria-hidden="true" />
                </Item>
              ))
            : rows.map((env) => (
                <StackedRow
                  key={env.id}
                  env={env}
                  finish={finish(env)}
                  onFinishSetup={onFinishSetup}
                  flash={flashing.has(env.id)}
                />
              ))}
        </ItemGroup>
      ) : (
        // The wrapper scrolls sideways on a narrow screen and down past the
        // page height, so the sticky header has something to stick to.
        <div
          className="relative max-h-[calc(100dvh-12rem)] overflow-x-auto overflow-y-auto rounded-xl ring-1 ring-foreground/10"
          {...region}
        >
          <table
            data-slot="table"
            aria-busy={loading || undefined}
            aria-label={t("overview.tableLabel")}
            className="w-full caption-bottom text-sm"
          >
            <Header sort={sort} dir={dir} onSortChange={onSortChange} />
            <TableBody>
              {loading
                ? Array.from({ length: SKELETON_ROWS }, (_, i) => (
                    // Placeholder rows have no identity of their own.
                    // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows
                    <TableRow key={i} className={ROW}>
                      {COLUMNS.map((column) => (
                        <TableCell key={column} className="px-2 py-0">
                          <Skeleton className="h-4 w-20" aria-hidden="true" />
                        </TableCell>
                      ))}
                    </TableRow>
                  ))
                : rows.map((env) => {
                    const flash = flashing.has(env.id);
                    return (
                      <TableRow
                        key={env.id}
                        data-flash={flash || undefined}
                        className={cn(ROW, "transition-colors duration-150", flash && "bg-muted")}
                      >
                        <TableCell className="px-2 py-0">
                          <HealthCell env={env} />
                        </TableCell>
                        <TableCell className="px-2 py-0">
                          <NameCell env={env} finish={finish(env)} onFinishSetup={onFinishSetup} />
                        </TableCell>
                        <TableCell className="px-2 py-0">
                          <span className="inline-flex items-center gap-1">
                            <TypeBadge
                              envType={env.envType}
                              type={env.type}
                              isLocalSocket={env.isLocalSocket}
                            />
                          </span>
                        </TableCell>
                        <TableCell className="px-2 py-0">
                          <NodesCell env={env} />
                        </TableCell>
                        <TableCell className="px-2 py-0">
                          <FailingCell env={env} />
                        </TableCell>
                        <TableCell className="px-2 py-0">
                          <AgentsCell env={env} />
                        </TableCell>
                        <TableCell className="px-2 py-0 tabular-nums">
                          {env.engineVersion ? env.engineVersion : <Dash />}
                        </TableCell>
                        <TableCell className="px-2 py-0">
                          <AgentCell env={env} />
                        </TableCell>
                        <TableCell className="px-2 py-0 text-muted-foreground">
                          <LastSeenCell env={env} />
                        </TableCell>
                      </TableRow>
                    );
                  })}
            </TableBody>
          </table>
        </div>
      )}
    </>
  );
}

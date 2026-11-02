// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Check, ChevronsUpDown, Pause, Play, X } from "lucide-react";
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";

import { NoPermission } from "@/components/full-page-state";
import { LiveChip } from "@/components/live-chip";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { SearchBox } from "@/features/containers/container-list";
import {
  createEventsConnection,
  type EventDTO,
  type EventsConnection,
  type EventsSnapshot,
  fetchRecentEvents,
} from "@/features/dashboard/events";
import { type CanOpen, EventsTable } from "@/features/events/events-table";
import {
  applyEventFilters,
  DEFAULT_RANGE,
  DOCKER_TYPES,
  type DockerType,
  type EventsSearch,
  type EventsSearchPatch,
  RANGES,
  type Range,
  rangeStart,
  SWARM_TYPES,
} from "@/features/events/filters";
import { NodeSelect } from "@/features/resources/resource-table";
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { useEnvName } from "@/features/resources/use-env-name";
import { hasPermission, useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { fitToViewport } from "@/lib/fit-to-viewport";

// How many events the page keeps, newest first (D-48). Events are held in the
// browser only and are not stored on the server.
export const PAGE_CAP = 1000;

// How far down the table must be scrolled before new events are held back and the
// "Jump to latest" pill appears, so a table resting at the top keeps following.
const SCROLLED_PX = 8;

const INITIAL: EventsSnapshot = { events: [], state: "loading", ready: false };

interface History extends EventsSnapshot {
  retry: () => void;
}

// useEventsHistory is the page's own read of the environment's events: the
// events from `range` ago, then every new one, up to PAGE_CAP. It is a second
// subscription beside the layout's, which keeps the dashboard's feed at 50 and
// is the one that refreshes the lists, so this one invalidates nothing. A new
// range closes the connection and reads again. Nothing is opened until the
// person is known to hold environments.read.
function useEventsHistory(envId: string, range: Range, enabled: boolean): History {
  const [snapshot, setSnapshot] = useState<EventsSnapshot>(INITIAL);
  const [connection, setConnection] = useState<EventsConnection | undefined>();

  useEffect(() => {
    setSnapshot(INITIAL);
    if (!enabled) return;
    const next = createEventsConnection({
      envId,
      cap: PAGE_CAP,
      since: rangeStart(range, Date.now()),
      fetchRecent: (query) => fetchRecentEvents(envId, query),
      invalidate: () => {},
      onChange: setSnapshot,
    });
    setConnection(next);
    next.start();
    return () => {
      next.close();
      setConnection(undefined);
    };
  }, [envId, range, enabled]);

  return useMemo(
    () => ({ ...snapshot, retry: () => connection?.reload() }),
    [snapshot, connection],
  );
}

const uniqueSorted = (values: Iterable<string>) =>
  [...new Set(values)].sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));

interface ActionFilterProps {
  // The actions seen in the buffer, plus any chosen that are no longer in it.
  actions: readonly string[];
  value: readonly string[];
  onChange: (actions: string[]) => void;
}

// ActionFilter is the "Action" button and its list: the actions the buffer holds,
// each a checkmark, any number chosen.
function ActionFilter({ actions, value, onChange }: ActionFilterProps) {
  const t = useT();
  const [open, setOpen] = useState(false);
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button
          type="button"
          variant="outline"
          size="sm"
          aria-expanded={open}
          className="max-md:h-11"
        >
          {value.length > 0
            ? t("events.action.withCount", { count: value.length })
            : t("events.action.label")}
          <ChevronsUpDown aria-hidden="true" className="opacity-50" />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-64 p-0">
        <Command>
          <CommandInput placeholder={t("events.action.search")} />
          <CommandList className="max-h-80">
            <CommandEmpty>{t("events.action.none")}</CommandEmpty>
            <CommandGroup>
              {actions.map((a) => {
                const chosen = value.includes(a);
                return (
                  <CommandItem
                    key={a}
                    value={a}
                    data-checked={chosen ? "true" : undefined}
                    className="max-md:min-h-11"
                    onSelect={() => onChange(chosen ? value.filter((v) => v !== a) : [...value, a])}
                  >
                    <span className="min-w-0 flex-1 truncate">{a}</span>
                    {chosen ? <Check aria-hidden="true" /> : null}
                  </CommandItem>
                );
              })}
            </CommandGroup>
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}

interface EventsPageProps {
  envId: string;
  search: EventsSearch;
  onSearchChange: (patch: EventsSearchPatch) => void;
}

// EventsPage is the Events page of one environment (RES-10, D-48): what Docker
// reports, newest first, live. It needs environments.read, the permission the
// events stream already uses (D-51), and shows exactly the fields the server
// allows (D-61). While the account is still being read the page waits as a
// skeleton; it is the server that refuses, this only decides what to show (D-50).
export function EventsPage(props: EventsPageProps) {
  const { data: me } = useMe();
  if (me && !hasPermission(me, "environments.read", props.envId)) return <NoPermission />;
  // A new range is a new read of the history: everything the view held, the pause
  // and the scroll included, starts over.
  return (
    <EventsView
      key={props.search.since ?? DEFAULT_RANGE}
      {...props}
      allowed={hasPermission(me, "environments.read", props.envId)}
    />
  );
}

function EventsView({
  envId,
  search,
  onSearchChange,
  allowed,
}: EventsPageProps & { allowed: boolean }) {
  const t = useT();
  const envName = useEnvName(envId);
  const swarm = useEnvIsSwarm(envId);
  const { data: me } = useMe();
  const range = search.since ?? DEFAULT_RANGE;
  const { events, state, ready, retry } = useEventsHistory(envId, range, allowed);

  const canOpen: CanOpen = {
    container: hasPermission(me, "containers.read", envId),
    network: hasPermission(me, "networks.read", envId),
    volume: hasPermission(me, "volumes.read", envId),
  };

  // The type chips the environment has: the swarm kinds only on a swarm.
  const chips: readonly DockerType[] = swarm
    ? DOCKER_TYPES
    : DOCKER_TYPES.filter((type) => !SWARM_TYPES.includes(type));
  const types = (search.type ?? []).filter((type) => chips.includes(type));
  const node = swarm ? search.node : undefined;
  const filters = { types, actions: search.action, node, q: search.q, name: search.name };

  // Holding: while paused, or while the table is scrolled away from the top, the
  // rows on screen are the ones when the hold began and new events wait behind a
  // pill, so nothing moves under the pointer. The filters still apply to what is
  // held.
  const [paused, setPaused] = useState(false);
  const [scrolled, setScrolled] = useState(false);
  const scroller = useRef<HTMLDivElement>(null);
  // At most as tall as the window leaves room for, measured, so a long feed scrolls
  // inside the table and not the page as well.
  useLayoutEffect(() => fitToViewport(scroller.current, 256, "maxHeight"), []);
  const held = useRef<EventDTO[] | null>(null);
  const holding = paused || scrolled;
  if (!holding) held.current = null;
  else if (held.current === null) held.current = events;
  const base = holding && held.current ? held.current : events;
  const shown = applyEventFilters(base, filters);
  const waiting = holding
    ? (() => {
        const known = new Set(base.map((e) => e.id));
        return applyEventFilters(events, filters).filter((e) => !known.has(e.id)).length;
      })()
    : 0;

  function release() {
    setPaused(false);
    setScrolled(false);
    if (scroller.current) scroller.current.scrollTop = 0;
  }

  const chip = !paused && (state === "live" || state === "reconnecting") ? state : undefined;
  function clearFilters() {
    onSearchChange({
      type: undefined,
      action: undefined,
      node: undefined,
      q: undefined,
      name: undefined,
    });
  }

  let body: React.ReactNode;
  if (!ready && state === "error") {
    body = (
      <div role="alert" className="flex flex-col items-start gap-3 p-6 text-sm">
        <div>
          <p className="font-semibold">{t("events.error.title")}</p>
          <p className="text-muted-foreground">{t("events.error.body")}</p>
        </div>
        <Button variant="outline" size="sm" className="max-md:h-11" onClick={retry}>
          {t("events.error.retry")}
        </Button>
      </div>
    );
  } else if (!ready) {
    body = (
      <div aria-hidden="true" className="flex flex-col gap-1 p-3">
        {[0, 1, 2, 3, 4, 5, 6, 7].map((i) => (
          <Skeleton key={i} className="h-9 w-full" />
        ))}
      </div>
    );
  } else if (events.length === 0) {
    body = (
      <Empty className="border-0 p-8">
        <EmptyHeader>
          <EmptyTitle className="text-base">{t("events.empty.title")}</EmptyTitle>
          <EmptyDescription>{t("events.empty.body")}</EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  } else if (shown.length === 0) {
    body = (
      <Empty className="border-0 p-8">
        <EmptyHeader>
          <EmptyTitle className="text-base">{t("events.noMatch.title")}</EmptyTitle>
        </EmptyHeader>
        <EmptyContent>
          <Button variant="outline" className="max-md:h-11" onClick={clearFilters}>
            {t("events.noMatch.action")}
          </Button>
        </EmptyContent>
      </Empty>
    );
  } else {
    body = (
      <EventsTable
        envId={envId}
        events={shown}
        swarm={swarm}
        canOpen={canOpen}
        onFilterName={(name) => onSearchChange({ name })}
      />
    );
  }

  return (
    <div className="mx-auto flex w-full max-w-7xl flex-col gap-4">
      <header className="flex flex-col gap-1">
        <h1 className="text-2xl font-semibold">{t("events.title")}</h1>
        <p className="text-sm text-muted-foreground">{t("events.caption", { name: envName })}</p>
        <p className="text-sm text-muted-foreground">{t("events.info")}</p>
      </header>

      <div className="flex flex-wrap items-center gap-2 max-md:[&>*]:min-w-0">
        <div className="flex items-center gap-2">
          <Label htmlFor="events-range" className="text-sm text-muted-foreground">
            {t("events.range.label")}
          </Label>
          <Select
            value={range}
            onValueChange={(v) =>
              onSearchChange({ since: v === DEFAULT_RANGE ? undefined : (v as Range) })
            }
          >
            <SelectTrigger id="events-range" size="sm" className="min-w-40 max-md:h-11">
              <SelectValue />
            </SelectTrigger>
            <SelectContent position="popper" align="start">
              {RANGES.map((r) => (
                <SelectItem key={r} value={r}>
                  {t(`events.range.${r}`)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        <ToggleGroup
          type="multiple"
          variant="outline"
          size="sm"
          value={types}
          aria-label={t("events.types.label")}
          onValueChange={(v) =>
            onSearchChange({ type: v.length > 0 ? (v as DockerType[]) : undefined })
          }
          className="flex-wrap justify-start"
        >
          {chips.map((type) => (
            <ToggleGroupItem key={type} value={type} className="min-h-9 max-md:min-h-11">
              {t(`events.types.${type}`)}
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
        <ActionFilter
          actions={uniqueSorted([
            ...events.map((e) => e.action).filter(Boolean),
            ...(search.action ?? []),
          ])}
          value={search.action ?? []}
          onChange={(a) => onSearchChange({ action: a.length > 0 ? a : undefined })}
        />
        {swarm ? (
          <NodeSelect
            value={node}
            onChange={(n) => onSearchChange({ node: n })}
            nodes={uniqueSorted(events.map((e) => e.node).filter(Boolean))}
          />
        ) : null}
        <SearchBox
          value={search.q}
          onCommit={(q) => onSearchChange({ q })}
          label={t("events.search.label")}
          placeholder={t("events.search.placeholder")}
          clearLabel={t("events.search.clear")}
        />
        {search.name !== undefined ? (
          <Badge variant="outline" className="h-7 max-w-full gap-1 pr-1 max-md:h-11">
            <span className="min-w-0 truncate">
              {t("events.nameChip.label", { name: search.name })}
            </span>
            <Button
              type="button"
              variant="ghost"
              size="icon-xs"
              className="size-5 max-md:size-9"
              aria-label={t("events.nameChip.remove")}
              onClick={() => onSearchChange({ name: undefined })}
            >
              <X aria-hidden="true" />
            </Button>
          </Badge>
        ) : null}
        <div className="ml-auto flex items-center gap-2">
          {chip ? <LiveChip state={chip} /> : null}
          {paused ? <Badge variant="outline">{t("events.paused")}</Badge> : null}
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            aria-pressed={paused}
            onClick={() => (paused ? release() : setPaused(true))}
          >
            {paused ? <Play aria-hidden="true" /> : <Pause aria-hidden="true" />}
            {paused ? t("events.resume") : t("events.pause")}
          </Button>
        </div>
      </div>

      <Card className="min-w-0 gap-0 py-0">
        <div
          ref={scroller}
          onScroll={(e) => setScrolled(e.currentTarget.scrollTop > SCROLLED_PX)}
          className="min-h-64 overflow-auto"
        >
          {/* The pills sit over the table's first row and take no room of their own, so a
              pill appearing moves nothing. */}
          {waiting > 0 ? (
            <div className="sticky top-2 z-20 h-0">
              <div
                className={paused ? "absolute left-2" : "absolute inset-x-0 flex justify-center"}
              >
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="rounded-full bg-background shadow-xs max-md:h-11"
                  onClick={release}
                >
                  {paused
                    ? t("events.newCount", { count: waiting })
                    : t("events.jump", { count: waiting })}
                </Button>
              </div>
            </div>
          ) : null}
          {body}
        </div>
        {/* Said once when the count changes, never per row. */}
        <span role="status" aria-live="polite" className="sr-only">
          {waiting > 0 ? t("events.newCount", { count: waiting }) : ""}
        </span>
        {ready && events.length >= PAGE_CAP ? (
          <p className="border-t px-4 py-2 text-xs text-muted-foreground">
            {t("events.bufferNote")}
          </p>
        ) : null}
      </Card>
    </div>
  );
}

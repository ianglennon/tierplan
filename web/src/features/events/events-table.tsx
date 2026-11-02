// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { ChevronRight, MoreHorizontal } from "lucide-react";
import { Fragment, memo, useCallback, useRef, useState } from "react";
import { toast } from "sonner";

import { containersQuery, networksQuery, volumesQuery } from "@/api/resources";
import { KeyValueTable } from "@/components/key-value-table";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import type { EventDTO } from "@/features/dashboard/events";
import { isGone } from "@/features/events/filters";
import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";
import { formatAbsolute } from "@/lib/format";
import { cn } from "@/lib/utils";

// How many characters of an ID stand in for a name Docker did not give.
const SHORT_ID = 12;

const CLOCK = new Intl.DateTimeFormat(undefined, {
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hourCycle: "h23",
});
const DAY = new Intl.DateTimeFormat(undefined, { dateStyle: "full" });

// The browser's own day of a time, so rows are cut into days as the person
// living in that zone counts them.
function dayKey(time: string): string {
  const d = new Date(time);
  return `${d.getFullYear()}-${d.getMonth()}-${d.getDate()}`;
}

// The details of a row: up to two facts, the image and then the exit code.
function detailsOf(event: EventDTO, t: ReturnType<typeof useT>): string[] {
  const exitCode = event.attributes.exitCode;
  return [
    event.attributes.image,
    exitCode === undefined ? undefined : t("events.row.exitCode", { code: exitCode }),
  ].filter((a): a is string => Boolean(a));
}

// The kinds of object that have a page of their own in this phase.
type Openable = "container" | "network" | "volume";

// Which of those pages the person may open (containers.read, networks.read and
// volumes.read). A name is a link only to a page they can see.
export type CanOpen = Record<Openable, boolean>;

function openable(event: EventDTO, can: CanOpen): event is EventDTO & { type: Openable } {
  if (isGone(event.action) || event.name === "") return false;
  return (
    (event.type === "container" || event.type === "network" || event.type === "volume") &&
    can[event.type]
  );
}

const LINK_CLASS =
  "block min-w-0 rounded-sm text-left text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none";

interface Placed {
  node: string;
  nodeId: string;
}

// Of the rows a list gave back, the one the event is about: the object itself, on
// the event's node if it is on several (a swarm network is listed once per node).
function placeOf<T extends Placed>(
  rows: readonly T[] | null,
  same: (row: T) => boolean,
  event: EventDTO,
): string | undefined {
  const mine = (rows ?? []).filter(same);
  return (mine.find((r) => r.node === event.node) ?? mine[0])?.nodeId;
}

// On a swarm a detail page is found by the ID of the node, which an event does not
// carry, only its name. The list of that kind, narrowed by the name, says which
// node ID the object is on (two hosts can share a display name, so the object's
// own ID decides). Undefined means the object is no longer there.
async function nodeIdOf(
  queryClient: QueryClient,
  envId: string,
  event: EventDTO & { type: Openable },
): Promise<string | undefined> {
  const fresh = { staleTime: 0 };
  switch (event.type) {
    case "container": {
      const list = await queryClient.fetchQuery({
        ...containersQuery(envId, { q: event.name }),
        ...fresh,
      });
      return placeOf(list.items, (c) => c.id === event.actorId, event);
    }
    case "network": {
      const list = await queryClient.fetchQuery({
        ...networksQuery(envId, { q: event.name }),
        ...fresh,
      });
      return placeOf(list.items, (n) => n.id === event.actorId, event);
    }
    case "volume": {
      const list = await queryClient.fetchQuery({
        ...volumesQuery(envId, { q: event.name }),
        ...fresh,
      });
      return placeOf(list.items, (v) => v.name === event.name, event);
    }
  }
}

interface NameLinkProps {
  envId: string;
  event: EventDTO & { type: Openable };
  swarm: boolean;
}

// NameLink opens the page of the container, network or volume an event is about.
// The address is built from the route's own parameters, never from an attribute
// (T-04-102). Off a swarm there is one node and the page needs none; on a swarm
// the node's ID is looked up when the name is clicked.
function NameLink({ envId, event, swarm }: NameLinkProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [busy, setBusy] = useState(false);
  const [gone, setGone] = useState(false);
  const label = <TruncatedText mono>{event.name}</TruncatedText>;

  if (gone) return <TruncatedText mono>{event.name}</TruncatedText>;

  if (!swarm) {
    switch (event.type) {
      case "container":
        return (
          <Link
            to="/env/$envId/containers/$containerId"
            params={{ envId, containerId: event.actorId }}
            className={LINK_CLASS}
          >
            {label}
          </Link>
        );
      case "network":
        return (
          <Link
            to="/env/$envId/networks/$networkId"
            params={{ envId, networkId: event.actorId }}
            className={LINK_CLASS}
          >
            {label}
          </Link>
        );
      case "volume":
        return (
          <Link
            to="/env/$envId/volumes/$volumeName"
            params={{ envId, volumeName: event.name }}
            className={LINK_CLASS}
          >
            {label}
          </Link>
        );
    }
  }

  async function open() {
    if (busy) return;
    setBusy(true);
    try {
      const node = await nodeIdOf(queryClient, envId, event);
      if (node === undefined) {
        setGone(true);
        return;
      }
      switch (event.type) {
        case "container":
          await navigate({
            to: "/env/$envId/containers/$containerId",
            params: { envId, containerId: event.actorId },
            search: { node },
          });
          break;
        case "network":
          await navigate({
            to: "/env/$envId/networks/$networkId",
            params: { envId, networkId: event.actorId },
            search: { node },
          });
          break;
        case "volume":
          await navigate({
            to: "/env/$envId/volumes/$volumeName",
            params: { envId, volumeName: event.name },
            search: { node },
          });
          break;
      }
    } catch {
      toast.error(t("events.row.openFailed"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <button type="button" aria-busy={busy} className={LINK_CLASS} onClick={() => void open()}>
      {label}
    </button>
  );
}

interface RowsProps {
  envId: string;
  event: EventDTO;
  // True for the first row and where the date changes: the row gets a divider.
  newDay: boolean;
  swarm: boolean;
  canContainer: boolean;
  canNetwork: boolean;
  canVolume: boolean;
  expanded: boolean;
  onToggle: (id: string) => void;
  onFilterName: (name: string) => void;
}

// EventRows is one event: its row, the day divider above it where the date
// changes, and the attributes below it when it is open. It is memoised, so a new
// event at the top does not draw the other 999 again.
const EventRows = memo(function EventRows({
  envId,
  event,
  newDay,
  swarm,
  canContainer,
  canNetwork,
  canVolume,
  expanded,
  onToggle,
  onFilterName,
}: RowsProps) {
  const t = useT();
  const columns = swarm ? 8 : 7;
  const own = event.type === "tierplan";
  const subject = event.name || event.actorId.slice(0, SHORT_ID);
  const attributes = expanded
    ? Object.entries(event.attributes).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    : [];
  const canOpen: CanOpen = { container: canContainer, network: canNetwork, volume: canVolume };
  return (
    <Fragment>
      {newDay ? (
        <TableRow className="bg-muted/40 hover:bg-muted/40">
          <TableCell
            colSpan={columns}
            className="h-8 py-0 text-xs font-medium text-muted-foreground"
          >
            {DAY.format(new Date(event.time))}
          </TableCell>
        </TableRow>
      ) : null}
      <TableRow className="h-9">
        <TableCell className="py-0">
          {own ? null : (
            <Button
              type="button"
              variant="ghost"
              size="icon-xs"
              className="max-md:size-11"
              aria-expanded={expanded}
              aria-label={t("events.row.expand", { name: subject })}
              onClick={() => onToggle(event.id)}
            >
              <ChevronRight
                aria-hidden="true"
                className={cn("transition-transform", expanded && "rotate-90")}
              />
            </Button>
          )}
        </TableCell>
        <TableCell className="py-0 text-xs text-muted-foreground tabular-nums">
          <Tooltip>
            <TooltipTrigger asChild>
              <time dateTime={event.time}>{CLOCK.format(new Date(event.time))}</time>
            </TooltipTrigger>
            <TooltipContent>{formatAbsolute(event.time)}</TooltipContent>
          </Tooltip>
        </TableCell>
        <TableCell className="py-0">{t(`events.types.${event.type}`)}</TableCell>
        <TableCell className="py-0">{own ? "" : event.action}</TableCell>
        <TableCell className="py-0">
          {openable(event, canOpen) ? (
            <NameLink envId={envId} event={event} swarm={swarm} />
          ) : (
            <TruncatedText mono={!own} className="max-w-64">
              {subject}
            </TruncatedText>
          )}
        </TableCell>
        {swarm ? (
          <TableCell className="py-0">
            <TruncatedText className="max-w-40 text-xs text-muted-foreground">
              {event.node}
            </TruncatedText>
          </TableCell>
        ) : null}
        <TableCell className="py-0 text-xs text-muted-foreground">
          {own ? null : (
            <span className="flex min-w-0 gap-x-3">
              {detailsOf(event, t).map((a) => (
                <span key={a} className="min-w-0 truncate">
                  {a}
                </span>
              ))}
            </span>
          )}
        </TableCell>
        <TableCell className="py-0">
          {own || event.name === "" ? null : (
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-xs"
                  className="max-md:size-11"
                  aria-label={t("events.row.menu", { name: subject })}
                >
                  <MoreHorizontal aria-hidden="true" />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onSelect={() => onFilterName(event.name)}>
                  {t("events.row.filterByName")}
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          )}
        </TableCell>
      </TableRow>
      {expanded ? (
        <TableRow className="hover:bg-transparent">
          <TableCell />
          <TableCell colSpan={columns - 1} className="whitespace-normal">
            <KeyValueTable
              rows={attributes.map(([key, value]) => ({ key, value }))}
              empty={t("events.row.noAttributes")}
              label={t("events.row.attributesLabel", { name: subject })}
            />
          </TableCell>
        </TableRow>
      ) : null}
    </Fragment>
  );
});

interface EventsTableProps {
  envId: string;
  // Newest first.
  events: readonly EventDTO[];
  // Whether each row names its node (a swarm).
  swarm: boolean;
  canOpen: CanOpen;
  // The row menu's "Filter by this name".
  onFilterName: (name: string) => void;
}

// EventsTable is the rows of Docker events, newest first, with a divider where
// the day changes and a row that opens to every attribute the server allowed.
// Every name and attribute comes from a daemon and is drawn as text only
// (T-04-102, T-04-103).
export function EventsTable({ envId, events, swarm, canOpen, onFilterName }: EventsTableProps) {
  const t = useT();
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());
  const toggle = useCallback((id: string) => {
    setOpen((prev) => {
      const next = new Set(prev);
      if (!next.delete(id)) next.add(id);
      return next;
    });
  }, []);
  // The rows are memoised, so what they are given must not change with every
  // render of the page: the latest callback is read through a ref.
  const filterName = useRef(onFilterName);
  filterName.current = onFilterName;
  const filter = useCallback((name: string) => filterName.current(name), []);

  return (
    // Under 768px the columns keep their width and the card scrolls sideways,
    // instead of squeezing every cell to a few letters.
    <Table aria-label={t("events.table.label")} className="min-w-[48rem]">
      <TableHeader>
        <TableRow>
          <TableHead className="w-11">
            <span className="sr-only">{t("events.columns.expand")}</span>
          </TableHead>
          <TableHead className="w-24">{t("events.columns.time")}</TableHead>
          <TableHead className="w-28">{t("events.columns.type")}</TableHead>
          <TableHead className="w-32">{t("events.columns.action")}</TableHead>
          <TableHead className="min-w-40">{t("events.columns.name")}</TableHead>
          {swarm ? <TableHead className="w-44">{t("events.columns.node")}</TableHead> : null}
          <TableHead className="min-w-40">{t("events.columns.details")}</TableHead>
          <TableHead className="w-11">
            <span className="sr-only">{t("events.columns.menu")}</span>
          </TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {events.map((event, i) => {
          const previous = events[i - 1];
          return (
            <EventRows
              key={event.id}
              envId={envId}
              event={event}
              newDay={previous === undefined || dayKey(previous.time) !== dayKey(event.time)}
              swarm={swarm}
              canContainer={canOpen.container}
              canNetwork={canOpen.network}
              canVolume={canOpen.volume}
              expanded={open.has(event.id)}
              onToggle={toggle}
              onFilterName={filter}
            />
          );
        })}
      </TableBody>
    </Table>
  );
}

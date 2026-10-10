// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Fragment } from "react";

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
import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";
import { formatAbsolute } from "@/lib/format";

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

interface EventsTableProps {
  // Newest first.
  events: readonly EventDTO[];
  // Whether each row names its node (a swarm).
  swarm: boolean;
}

// EventsTable is the rows of Docker events, newest first, with a divider where
// the day changes. Every name and attribute comes from a daemon and is drawn as
// text only (T-04-102).
export function EventsTable({ events, swarm }: EventsTableProps) {
  const t = useT();
  return (
    <Table aria-label={t("events.table.label")}>
      <TableHeader>
        <TableRow>
          <TableHead className="w-24">{t("events.columns.time")}</TableHead>
          <TableHead className="w-28">{t("events.columns.type")}</TableHead>
          <TableHead className="w-32">{t("events.columns.action")}</TableHead>
          <TableHead>{t("events.columns.name")}</TableHead>
          {swarm ? <TableHead className="w-44">{t("events.columns.node")}</TableHead> : null}
          <TableHead>{t("events.columns.details")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {events.map((event, i) => {
          const previous = events[i - 1];
          const newDay = previous === undefined || dayKey(previous.time) !== dayKey(event.time);
          const own = event.type === "bayplan";
          const subject = event.name || event.actorId.slice(0, SHORT_ID);
          return (
            <Fragment key={event.id}>
              {newDay ? (
                <TableRow className="bg-muted/40 hover:bg-muted/40">
                  <TableCell
                    colSpan={swarm ? 6 : 5}
                    className="h-8 py-0 text-xs font-medium text-muted-foreground"
                  >
                    {DAY.format(new Date(event.time))}
                  </TableCell>
                </TableRow>
              ) : null}
              <TableRow className="h-9">
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
                  <TruncatedText mono={!own} className="max-w-64">
                    {subject}
                  </TruncatedText>
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
              </TableRow>
            </Fragment>
          );
        })}
      </TableBody>
    </Table>
  );
}

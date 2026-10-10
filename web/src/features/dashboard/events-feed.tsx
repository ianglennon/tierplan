// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import {
  Box,
  Boxes,
  FileText,
  HardDrive,
  KeyRound,
  Layers,
  type LucideIcon,
  Network,
  Server,
} from "lucide-react";
import { useState } from "react";

import { LiveChip } from "@/components/live-chip";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Card, CardAction, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Skeleton } from "@/components/ui/skeleton";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { type EventDTO, type EventType, useEnvEvents } from "@/features/dashboard/events";
import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";

// A row of Bayplan's own, such as a manager failover, has no Docker type and so no
// icon: a label stands in. It has no filter either and shows under All only.
type DockerEventType = Exclude<EventType, "bayplan">;
type Filter = "all" | DockerEventType;

const FILTERS: readonly Filter[] = ["all", "container", "image", "network", "volume"];

const ICONS: Record<DockerEventType, LucideIcon> = {
  container: Box,
  image: Layers,
  network: Network,
  volume: HardDrive,
  service: Boxes,
  node: Server,
  secret: KeyRound,
  config: FileText,
};

// How many characters of an ID stand in for a name Docker did not give.
const SHORT_ID = 12;

// EventRow is one event: when, what kind, what happened, to what, and up to two
// facts about it (image, then exit code). Names come from the daemon and are
// rendered as text only (T-01-91).
function EventRow({ event, showNode }: { event: EventDTO; showNode?: boolean }) {
  const t = useT();
  if (event.type === "bayplan") {
    // The message is written by Bayplan, from names agents reported; rendered as text.
    return (
      <li className="flex items-start gap-3 border-b py-2 last:border-b-0">
        <span className="w-20 shrink-0 pt-0.5 text-xs text-muted-foreground">
          <RelativeTime value={event.time} />
        </span>
        {/* Keeps the message in line with the other rows' text, where the icon is. */}
        <span className="w-4 shrink-0" aria-hidden="true" />
        <p className="min-w-0 flex-1 text-sm font-semibold">
          <span className="mr-2 text-xs font-semibold text-muted-foreground">
            {t("dashboard.events.filters.bayplan")}
          </span>
          {event.name}
        </p>
      </li>
    );
  }
  const Icon = ICONS[event.type];
  const subject = event.name || event.actorId.slice(0, SHORT_ID);
  const exitCode = event.attributes.exitCode;
  const attributes = [
    event.attributes.image,
    exitCode === undefined ? undefined : t("dashboard.events.exitCode", { code: exitCode }),
  ].filter((a): a is string => Boolean(a));

  return (
    <li className="flex items-start gap-3 border-b py-2 last:border-b-0">
      <span className="w-20 shrink-0 pt-0.5 text-xs text-muted-foreground">
        <RelativeTime value={event.time} />
      </span>
      <Icon className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
      <div className="flex min-w-0 flex-1 flex-col gap-0.5">
        <p className="text-sm font-semibold">
          {t("dashboard.events.rowTitle", {
            type: t(`dashboard.events.filters.${event.type}`),
            action: event.action,
          })}
        </p>
        <div className="flex min-w-0 flex-wrap items-baseline gap-x-3 gap-y-0.5">
          <TruncatedText mono className="max-w-full sm:max-w-64">
            {subject}
          </TruncatedText>
          {showNode && event.node ? (
            // Where it happened, on a swarm. An agent chose the name: text only.
            <TruncatedText className="max-w-40 text-xs text-muted-foreground">
              {event.node}
            </TruncatedText>
          ) : null}
          {attributes.map((a) => (
            <span key={a} className="min-w-0 truncate text-xs text-muted-foreground">
              {a}
            </span>
          ))}
        </div>
      </div>
    </li>
  );
}

// EventsFeed is the dashboard's "Recent events" card (D-55, D-57): the last
// hour best effort, then new events at the top as they happen, 50 at most.
// Filtering is client-side over what the provider holds. On a swarm each row
// names its node (D-43).
export function EventsFeed({ showNode }: { showNode?: boolean }) {
  const t = useT();
  const { events, state, ready, retry } = useEnvEvents();
  const [filter, setFilter] = useState<Filter>("all");

  const shown = filter === "all" ? events : events.filter((e) => e.type === filter);
  const chip = state === "live" || state === "reconnecting" ? state : undefined;

  let body: React.ReactNode;
  if (!ready && state === "error") {
    body = (
      <div role="alert" className="flex flex-wrap items-center gap-3 text-sm">
        <span>{t("dashboard.events.error")}</span>
        <Button variant="outline" size="sm" onClick={retry}>
          {t("dashboard.events.retry")}
        </Button>
      </div>
    );
  } else if (!ready) {
    body = (
      <div aria-hidden="true" className="flex flex-col gap-3">
        {[0, 1, 2, 3, 4].map((i) => (
          <Skeleton key={i} className="h-9 w-full" />
        ))}
      </div>
    );
  } else if (shown.length === 0) {
    const none = events.length === 0;
    body = (
      <Empty className="border-0 p-6">
        <EmptyHeader>
          <EmptyTitle className="text-base">
            {none ? t("dashboard.events.empty") : t("dashboard.events.noMatch")}
          </EmptyTitle>
          <EmptyDescription>
            {none ? t("dashboard.events.emptyBody") : t("dashboard.events.noMatchBody")}
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  } else {
    body = (
      <ScrollArea className="[&_[data-slot=scroll-area-viewport]]:max-h-[480px]">
        <ol>
          {shown.map((e) => (
            <EventRow key={e.id} event={e} showNode={showNode} />
          ))}
        </ol>
      </ScrollArea>
    );
  }

  return (
    <Card className="min-w-0">
      <CardHeader>
        <CardTitle className="text-base font-semibold">{t("dashboard.events.title")}</CardTitle>
        <CardAction className="flex flex-wrap items-center justify-end gap-2">
          {chip ? <LiveChip state={chip} /> : null}
        </CardAction>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        <ToggleGroup
          type="single"
          variant="outline"
          size="sm"
          value={filter}
          aria-label={t("dashboard.events.filterLabel")}
          // Deselecting the open item reports "": keep the filter instead.
          onValueChange={(v) => {
            if (v) setFilter(v as Filter);
          }}
          className="flex-wrap justify-start"
        >
          {FILTERS.map((f) => (
            <ToggleGroupItem key={f} value={f} className="min-h-9">
              {t(`dashboard.events.filters.${f}`)}
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
        {body}
      </CardContent>
    </Card>
  );
}

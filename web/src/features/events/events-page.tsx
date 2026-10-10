// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useEffect, useMemo, useState } from "react";

import { NoPermission } from "@/components/full-page-state";
import { LiveChip } from "@/components/live-chip";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import {
  createEventsConnection,
  type EventsConnection,
  type EventsSnapshot,
  fetchRecentEvents,
} from "@/features/dashboard/events";
import { EventsTable } from "@/features/events/events-table";
import {
  DEFAULT_RANGE,
  type EventsSearch,
  type EventsSearchPatch,
  type Range,
  rangeStart,
} from "@/features/events/filters";
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { useEnvName } from "@/features/resources/use-env-name";
import { hasPermission, useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// How many events the page keeps, newest first (D-48). Events are held in the
// browser only and are not stored on the server.
export const PAGE_CAP = 1000;

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
    // Nothing is opened for someone who may not read the environment.
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

interface EventsPageProps {
  envId: string;
  search: EventsSearch;
  onSearchChange: (patch: EventsSearchPatch) => void;
}

// EventsPage is the Events page of one environment (RES-10, D-48): what Docker
// reports, newest first, live. It needs environments.read, the permission the
// events stream already uses (D-51), and shows exactly the fields the server
// allows (D-61).
export function EventsPage({ envId, search }: EventsPageProps) {
  const t = useT();
  const { data: me } = useMe();
  const allowed = hasPermission(me, "environments.read", envId);
  const envName = useEnvName(envId);
  const swarm = useEnvIsSwarm(envId);
  const range = search.since ?? DEFAULT_RANGE;
  const { events, state, ready, retry } = useEventsHistory(envId, range, allowed);

  // While the account is still being read the page waits as a skeleton; it is
  // the server that refuses, this only decides what to show (D-50).
  if (me && !allowed) return <NoPermission />;

  const chip = state === "live" || state === "reconnecting" ? state : undefined;

  let body: React.ReactNode;
  if (!ready && state === "error") {
    body = (
      <div role="alert" className="flex flex-col items-start gap-3 p-6 text-sm">
        <div>
          <p className="font-semibold">{t("events.error.title")}</p>
          <p className="text-muted-foreground">{t("events.error.body")}</p>
        </div>
        <Button variant="outline" size="sm" onClick={retry}>
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
  } else {
    body = <EventsTable events={events} swarm={swarm} />;
  }

  return (
    <div className="mx-auto flex w-full max-w-7xl flex-col gap-4">
      <header className="flex flex-col gap-1">
        <h1 className="text-2xl font-semibold">{t("events.title")}</h1>
        <p className="text-sm text-muted-foreground">{t("events.caption", { name: envName })}</p>
        <p className="text-sm text-muted-foreground">{t("events.info")}</p>
      </header>
      <div className="flex flex-wrap items-center gap-2">
        {chip ? <LiveChip state={chip} /> : null}
      </div>
      <Card className="min-w-0 gap-0 py-0">
        <div className="max-h-[calc(100svh-18rem)] min-h-64 overflow-auto">{body}</div>
        {ready && events.length >= PAGE_CAP ? (
          <p className="border-t px-4 py-2 text-xs text-muted-foreground">
            {t("events.bufferNote")}
          </p>
        ) : null}
      </Card>
    </div>
  );
}

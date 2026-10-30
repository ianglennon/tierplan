// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import { EventsPage } from "@/features/events/events-page";
import { type EventsSearchPatch, eventsSearchSchema } from "@/features/events/filters";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

function Events() {
  const { envId } = Route.useParams();
  const t = useT();
  useDocumentTitle(t("events.title"));
  const search = Route.useSearch();
  const navigate = Route.useNavigate();

  // A change to the filters replaces the history entry, so the back button leaves
  // the page and does not walk through every chip.
  function change(patch: EventsSearchPatch) {
    void navigate({
      search: (prev) => {
        const next: Record<string, unknown> = { ...prev, ...patch };
        for (const key of Object.keys(next)) {
          const value = next[key];
          if (value === undefined || (Array.isArray(value) && value.length === 0)) delete next[key];
        }
        return next;
      },
      replace: true,
    });
  }

  return <EventsPage envId={envId} search={search} onSearchChange={change} />;
}

export const Route = createFileRoute("/_app/env/$envId/events")({
  validateSearch: eventsSearchSchema,
  component: Events,
});

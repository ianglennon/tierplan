// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useIsMutating, useQuery } from "@tanstack/react-query";
import { CircleCheck, TriangleAlert } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { serviceActionKey, serviceRolloutQuery } from "@/api/services";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Progress } from "@/components/ui/progress";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// How long "Update complete" stays before the region collapses (UI-SPEC "Rollout region"). It is
// an object so a test can shorten it.
export const rolloutTiming = { completeMs: 4000 };

type Announcement = "complete" | "paused" | "rollbackPaused" | "rolledBack";

interface RolloutProgressProps {
  envId: string;
  serviceId: string;
  // The service's name, for the title. Text from Docker, drawn as text.
  name: string;
  // Open the rollback dialog (an update that paused).
  onRollback: () => void;
  // Open the force update dialog in retry mode.
  onRetry: () => void;
}

// RolloutProgress is the inline region for an update or a rollback: while the swarm replaces
// tasks, the title, how many are on the new version and a progress bar; when it completes, an
// "Update complete" line for four seconds; when Docker pauses it or rolls it back, an alert with
// Docker's words and Roll back and Retry update, which stays until the next update (D-03, D-12).
// A change to complete, paused or rolled back is announced once, politely, by a live region that
// is always there; the alerts themselves are not live. Docker's message is text and is never
// linkified (T-06-29). A service with no update, or one that finished before the page was opened,
// draws nothing but the live region.
export function RolloutProgress({
  envId,
  serviceId,
  name,
  onRollback,
  onRetry,
}: RolloutProgressProps) {
  const t = useT();
  const needs = useNeeds("services.control", envId);
  const pending = useIsMutating({ mutationKey: serviceActionKey(envId) }) > 0;
  const { data } = useQuery(serviceRolloutQuery(envId, serviceId));
  const seen = useRef<{ state: string; startedAt: string | undefined } | undefined>(undefined);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  const [announced, setAnnounced] = useState<Announcement | undefined>(undefined);
  const [completeShown, setCompleteShown] = useState(false);

  useEffect(() => () => clearTimeout(timer.current), []);

  const state = data?.state;
  const startedAt = data?.startedAt;
  useEffect(() => {
    if (!state) return;
    const before = seen.current;
    seen.current = { state, startedAt };
    // The first answer is how things already were, not a change.
    if (!before) return;
    const changed = before.state !== state;
    if (state === "completed" && (before.state === "updating" || before.startedAt !== startedAt)) {
      setAnnounced("complete");
      setCompleteShown(true);
      clearTimeout(timer.current);
      timer.current = setTimeout(() => setCompleteShown(false), rolloutTiming.completeMs);
    } else if (changed && state === "paused") {
      setAnnounced("paused");
    } else if (changed && state === "rollback_paused") {
      setAnnounced("rollbackPaused");
    } else if (changed && state === "rollback_completed") {
      setAnnounced("rolledBack");
    }
  }, [state, startedAt]);

  const live = (
    <div data-testid="rollout-announcement" role="status" className="sr-only">
      {announced ? t(`servicedetail.rollout.announce.${announced}`) : ""}
    </div>
  );
  if (!data) return live;

  const message = data.message;
  const retry = (
    <NeedsTooltip reason={needs.reason}>
      <Button
        type="button"
        variant="outline"
        size="sm"
        className="max-md:h-11"
        disabled={pending}
        onClick={needs.allowed ? onRetry : undefined}
      >
        {t("servicedetail.rollout.retry")}
      </Button>
    </NeedsTooltip>
  );

  switch (data.state) {
    case "updating":
    case "rollback_started": {
      const rollingBack = data.state === "rollback_started";
      const base = rollingBack
        ? "servicedetail.rollout.rollingBack"
        : "servicedetail.rollout.updating";
      const percent = data.total > 0 ? Math.round((100 * data.updated) / data.total) : 0;
      return (
        <>
          {live}
          <section
            aria-label={t(`${base}.title`, { name })}
            className="flex flex-col gap-2 rounded-lg border bg-card px-3 py-2.5 text-sm"
          >
            <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
              <p className="min-w-0 font-medium break-all">{t(`${base}.title`, { name })}</p>
              <p className="text-muted-foreground tabular-nums">
                {t(`${base}.count`, { updated: data.updated, count: data.total })}
              </p>
            </div>
            <Progress
              value={percent}
              aria-valuenow={percent}
              aria-label={t("servicedetail.rollout.progressLabel")}
            />
            {message ? (
              <p className="text-xs break-words text-muted-foreground">{message}</p>
            ) : null}
          </section>
        </>
      );
    }
    case "completed":
      if (!completeShown) return live;
      return (
        <>
          {live}
          <p className="flex items-center gap-2 rounded-lg border bg-card px-3 py-2.5 text-sm">
            <CircleCheck aria-hidden="true" className="size-4 text-status-online-text" />
            {t("servicedetail.rollout.complete")}
          </p>
        </>
      );
    case "paused":
    case "rollback_paused":
    case "rollback_completed": {
      const base =
        data.state === "paused"
          ? "servicedetail.rollout.paused"
          : data.state === "rollback_paused"
            ? "servicedetail.rollout.rollbackPaused"
            : "servicedetail.rollout.rolledBack";
      return (
        <>
          {live}
          <Alert variant="warning" role="group" aria-label={t(`${base}.title`)}>
            <TriangleAlert aria-hidden="true" />
            <AlertTitle>{t(`${base}.title`)}</AlertTitle>
            <AlertDescription className="break-words">
              {message ? t(`${base}.body`, { message }) : t(`${base}.bodyBare`)}
            </AlertDescription>
            <div className="col-start-2 mt-2 flex flex-wrap gap-2">
              {data.state === "paused" ? (
                <NeedsTooltip reason={needs.reason}>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="max-md:h-11"
                    disabled={pending}
                    onClick={needs.allowed ? onRollback : undefined}
                  >
                    {t("servicedetail.rollout.rollBack")}
                  </Button>
                </NeedsTooltip>
              ) : null}
              {retry}
            </div>
          </Alert>
        </>
      );
    }
    default:
      return live;
  }
}

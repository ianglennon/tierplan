// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useEffect, useRef, useState } from "react";

import type { ImpactLoad, ImpactRow } from "@/api/teams";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useT } from "@/i18n/use-t";

// How many skeleton rows hold the place of the list while it loads.
const SKELETON_ROWS = 3;

type State = { kind: "loading" } | { kind: "error" } | { kind: "ok"; rows: ImpactRow[] };

interface ImpactPreviewProps {
  // Asks the server who would lose what. It is called once when the preview
  // opens, and again on Retry; a new function on a later render is not a reason
  // to ask again. Grant removal (plan 03-19) passes its own.
  load: () => Promise<ImpactLoad>;
  // Called with false while the preview is loading or has failed, and with true
  // once it has loaded (an empty list counts). The confirm that goes with the
  // preview stays aria-disabled until then, so a change is never confirmed
  // without its impact on screen: the preview fails closed.
  onReadyChange?: (ready: boolean) => void;
}

function roleLabel(t: (key: string) => string, role: string): string {
  return t(`teams.impact.role.${role}`);
}

// ImpactPreview is "What changes" (D-27): who loses which environment access if
// the change in front of the admin is confirmed, in the dialog that asks them to
// confirm it. It shows skeleton rows while loading, an inline alert with Retry
// when the preview fails, "No one loses access." when nothing changes, and
// otherwise one row per place access falls, inside a list that scrolls at 240px
// and is never truncated. A visually hidden polite live region announces the
// count once it has loaded. Usernames and environment names render as text only.
export function ImpactPreview({ load, onReadyChange }: ImpactPreviewProps) {
  const t = useT();
  const [state, setState] = useState<State>({ kind: "loading" });
  const [attempt, setAttempt] = useState(0);
  const latest = useRef({ load, onReadyChange });
  useEffect(() => {
    latest.current = { load, onReadyChange };
  });

  // biome-ignore lint/correctness/useExhaustiveDependencies: asks again only when attempt changes; the latest load and callback are read from the ref
  useEffect(() => {
    let current = true;
    setState({ kind: "loading" });
    latest.current.onReadyChange?.(false);
    latest.current
      .load()
      .then((result) => {
        if (!current) return;
        setState(result.kind === "ok" ? { kind: "ok", rows: result.rows } : { kind: "error" });
        latest.current.onReadyChange?.(result.kind === "ok");
      })
      .catch(() => {
        if (!current) return;
        setState({ kind: "error" });
        latest.current.onReadyChange?.(false);
      });
    return () => {
      current = false;
    };
  }, [attempt]);

  const rows = state.kind === "ok" ? state.rows : [];
  const people = new Set(rows.map((r) => r.userId)).size;
  const announcement =
    state.kind !== "ok"
      ? ""
      : people === 0
        ? t("teams.impact.noneAnnounce")
        : t("teams.impact.announce", { count: people });

  return (
    <section aria-labelledby="impact-preview-heading" className="flex min-w-0 flex-col gap-2">
      <h3 id="impact-preview-heading" className="text-sm font-semibold">
        {t("teams.impact.heading")}
      </h3>
      {state.kind === "loading" ? (
        <div aria-busy="true" className="flex flex-col gap-1.5">
          <span role="status" className="sr-only">
            {t("teams.impact.loading")}
          </span>
          {Array.from({ length: SKELETON_ROWS }, (_, i) => (
            // Placeholder rows have no identity of their own.
            // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows
            <Skeleton key={i} className="h-8 w-full max-md:h-11" aria-hidden="true" />
          ))}
        </div>
      ) : null}
      {state.kind === "error" ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("teams.impact.failed")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              onClick={() => setAttempt((n) => n + 1)}
            >
              {t("teams.impact.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      {state.kind === "ok" && rows.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t("teams.impact.none")}</p>
      ) : null}
      {state.kind === "ok" && rows.length > 0 ? (
        <ul
          aria-label={t("teams.impact.list")}
          className="flex max-h-60 flex-col overflow-y-auto rounded-md ring-1 ring-foreground/10"
        >
          {rows.map((r) => (
            <li
              key={`${r.userId}:${r.environmentId}`}
              className="grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto] items-center gap-3 px-3 py-1.5 text-sm odd:bg-muted/40 max-md:min-h-11"
            >
              <span className="truncate font-medium" title={r.username}>
                {r.username}
              </span>
              <span className="truncate text-muted-foreground" title={r.environmentName}>
                {r.environmentName}
              </span>
              <span className="whitespace-nowrap tabular-nums">
                {t("teams.impact.change", {
                  before: roleLabel(t, r.before),
                  after: roleLabel(t, r.after),
                })}
              </span>
            </li>
          ))}
        </ul>
      ) : null}
      {state.kind === "ok" ? (
        <p className="text-xs text-muted-foreground">{t("teams.impact.audit")}</p>
      ) : null}
      <div aria-live="polite" className="sr-only">
        {announcement}
      </div>
    </section>
  );
}

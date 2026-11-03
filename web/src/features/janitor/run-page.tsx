// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CircleCheck, CircleX, Info, Lock, TriangleAlert } from "lucide-react";
import { type ReactNode, useEffect, useId, useRef, useState } from "react";

import {
  JanitorRequestError,
  type JanitorRun,
  janitorRunKey,
  janitorRunQuery,
  runPollMs,
} from "@/api/janitor";
import { NoPermission } from "@/components/full-page-state";
import { LiveChip } from "@/components/live-chip";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Progress } from "@/components/ui/progress";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { SearchBox } from "@/features/containers/container-list";
import { countParts, durationText, formatSize } from "@/features/janitor/preview-summary";
import {
  RunItemsTable,
  type RunSearch,
  type RunSearchPatch,
  SHOW_VALUES,
  type Show,
} from "@/features/janitor/run-items-table";
import {
  type JanitorStreamState,
  progressOf,
  useJanitorRunStream,
} from "@/features/janitor/run-job-stream";
import { useEnvName } from "@/features/resources/use-env-name";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { formatAbsolute } from "@/lib/format";

// The icon of a run's state: always beside a word, never colour alone (UI-SPEC "Color").
function StateIcon({ state }: { state: JanitorRun["state"] }) {
  switch (state) {
    case "running":
      return <Spinner aria-hidden="true" />;
    case "waiting":
      return <Lock aria-hidden="true" className="size-4 text-status-warning-text" />;
    case "completed":
      return <CircleCheck aria-hidden="true" className="size-4 text-status-online-text" />;
    case "partial":
      return <TriangleAlert aria-hidden="true" className="size-4 text-status-warning-text" />;
    case "skipped":
      return <Info aria-hidden="true" className="size-4 text-muted-foreground" />;
    default:
      return <CircleX aria-hidden="true" className="size-4 text-status-offline-text" />;
  }
}

export interface RunPageProps {
  envId: string;
  runId: string;
  // The filters, as the address holds them. A page without an address keeps them itself.
  search?: RunSearch;
  onSearchChange?: (patch: RunSearchPatch) => void;
}

// RunPage is one run of the janitor, live or recorded (D-14): what started it, how it ended, what it
// counted, and every item it attempted with the outcome in words. While the run waits or works the
// page follows its job stream for the progress, and reads the run record from the server for its
// counts and items: every two seconds while the stream is not live, every five while it is, and at
// once when the plan becomes known and when the run ends. A run that has ended, or whose job the
// server no longer holds, is the record alone. It reads a run of this environment only; a run the
// history limit removed is a not-found page, not an error.
export function RunPage({ envId, runId, search, onSearchChange }: RunPageProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const readable = usePermission("environments.read", envId);
  const envName = useEnvName(envId);
  const [kept, setKept] = useState<RunSearch>({});
  const filters = search ?? kept;
  const changeFilters = (patch: RunSearchPatch) => {
    if (onSearchChange) onSearchChange(patch);
    else setKept((s) => ({ ...s, ...patch }));
  };
  const streamLive = useRef(false);
  const query = useQuery({
    ...janitorRunQuery(envId, runId),
    refetchInterval: (q) => runPollMs(q.state.data?.state, streamLive.current),
  });
  const run = query.data;
  const active = run?.state === "waiting" || run?.state === "running";
  const stream = useJanitorRunStream(envId, runId, active);
  streamLive.current = stream.status === "live" && !stream.finished;

  // The record is read again the moment the stream says the plan is known (a waiting run starts
  // working) or the job ended, instead of waiting for the next read.
  const planned = stream.job?.total ?? 0;
  const jobEnded = stream.finished;
  useEffect(() => {
    if (planned > 0 || jobEnded) {
      void queryClient.invalidateQueries({ queryKey: janitorRunKey(envId, runId) });
    }
  }, [planned, jobEnded, queryClient, envId, runId]);

  if (!readable) return <NoPermission />;
  const gone = query.error instanceof JanitorRequestError && query.error.notFound;

  if (gone) {
    return (
      <div className="mx-auto flex w-full max-w-7xl flex-col gap-4">
        <Crumb envId={envId} label={null} />
        <h1 className="text-2xl font-semibold">{t("janitorRun.notFound.title")}</h1>
        <Button asChild variant="outline" className="w-fit max-md:h-11">
          <Link to="/env/$envId/janitor" params={{ envId }} hash="history">
            {t("janitorRun.notFound.link")}
          </Link>
        </Button>
      </div>
    );
  }
  if (query.isError && !run) {
    return (
      <div className="mx-auto flex w-full max-w-7xl flex-col gap-4">
        <Crumb envId={envId} label={null} />
        <Alert variant="destructive">
          <AlertTitle>{t("janitorRun.error.title")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("janitorRun.error.body")}</span>
            <Button
              variant="outline"
              size="sm"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {query.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("janitorRun.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      </div>
    );
  }
  if (!run) {
    return (
      <div className="mx-auto flex w-full max-w-7xl flex-col gap-4" aria-busy="true">
        <Crumb envId={envId} label={null} />
        <h1 className="text-2xl font-semibold">{t("janitorRun.title", { env: envName })}</h1>
        <Skeleton className="h-4 w-80" />
        <Skeleton className="h-32 w-full" />
      </div>
    );
  }
  return (
    <RunBody
      envId={envId}
      envName={envName}
      run={run}
      stream={stream}
      search={filters}
      onSearchChange={changeFilters}
    />
  );
}

function Crumb({ envId, label }: { envId: string; label: ReactNode }) {
  const t = useT();
  return (
    <nav aria-label={t("janitorRun.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link
            to="/env/$envId/janitor"
            params={{ envId }}
            className="rounded-sm text-accent-ink hover:underline"
          >
            {t("janitorRun.breadcrumb.janitor")}
          </Link>
        </li>
        {label ? <li className="min-w-0">{label}</li> : null}
      </ol>
    </nav>
  );
}

// The LiveChip shows while the stream is connected or retrying for a run that is still going, and
// nowhere else: a page that has fallen back to reading the record claims nothing it cannot know.
function chipOf(run: JanitorRun, stream: JanitorStreamState): "live" | "reconnecting" | undefined {
  const going = run.state === "waiting" || run.state === "running";
  if (!going || stream.finished) return undefined;
  return stream.status === "live" || stream.status === "reconnecting" ? stream.status : undefined;
}

// RunHeader is the title with the outcome icon and word beside it, the live chip while the run is
// going, and the line that says what started the run and when it ended.
function RunHeader({
  envName,
  run,
  chip,
}: {
  envName: string;
  run: JanitorRun;
  chip: "live" | "reconnecting" | undefined;
}) {
  const t = useT();
  const started = formatAbsolute(run.startedAt);
  const seconds = run.finishedAt
    ? Math.max(0, (Date.parse(run.finishedAt) - Date.parse(run.startedAt)) / 1000)
    : undefined;
  return (
    <header className="flex flex-col gap-1">
      <h1 className="flex flex-wrap items-center gap-x-3 gap-y-1 text-2xl font-semibold">
        <span className="break-all">{t("janitorRun.title", { env: envName })}</span>
        <span className="inline-flex items-center gap-1.5 text-base">
          <StateIcon state={run.state} />
          {t(`janitorRun.state.${run.state}`)}
        </span>
        {chip ? <LiveChip state={chip} /> : null}
      </h1>
      <p className="text-xs text-muted-foreground">
        {run.trigger === "schedule"
          ? t("janitorRun.subtitle.schedule", { started })
          : t("janitorRun.subtitle.manual", { user: run.actorName, started })}{" "}
        {run.finishedAt && seconds !== undefined
          ? t("janitorRun.subtitle.finished", {
              finished: formatAbsolute(run.finishedAt),
              duration: durationText(seconds, t),
            })
          : null}
      </p>
    </header>
  );
}

// RunProgress is the one bar of a run that is going: how many removals have ended of the number the
// run planned, with the removal it is working on in mono. Its movement is not announced here; the
// announcer speaks once per quarter. It shows once the plan is known and the stream has a job.
function RunProgress({ run, stream }: { run: JanitorRun; stream: JanitorStreamState }) {
  const t = useT();
  const going = run.state === "waiting" || run.state === "running";
  const job = stream.job;
  if (!going || !job) return null;
  const { done, total } = progressOf(job);
  if (total === 0) return null;
  const current = job.items.find((i) => i.state === "running")?.label;
  return (
    <div className="flex flex-col gap-1.5">
      <Progress
        value={Math.floor((done / total) * 100)}
        aria-label={t("janitorRun.progressLabel")}
        aria-valuetext={t("janitorRun.progressValue", { done, total })}
      />
      <p aria-live="off" className="text-sm text-muted-foreground">
        {t("janitorRun.progress", { done, total })}
        {current ? (
          <>
            {" "}
            <span className="font-mono text-xs break-all text-foreground">{current}</span>
          </>
        ) : null}
      </p>
    </div>
  );
}

// RunAnnouncer is the page's one polite region. It speaks the progress once for each quarter of the
// run, and the end of a run that finished in front of the person once; it says nothing about a run
// as it was when the page opened, and it never moves the focus. A run that fails or is partly done
// is announced by its own alert.
function RunAnnouncer({ run, stream }: { run: JanitorRun; stream: JanitorStreamState }) {
  const t = useT();
  const seen = useRef<{ going: boolean; quarter: number } | null>(null);
  const [spoken, setSpoken] = useState<string[]>([]);
  const { done, total } = stream.job ? progressOf(stream.job) : { done: 0, total: 0 };
  const going = run.state === "waiting" || run.state === "running";
  const quarter = total > 0 ? Math.floor((done * 4) / total) : 0;
  const state = run.state;
  const counts = run.counts;
  useEffect(() => {
    const before = seen.current;
    seen.current = { going, quarter: Math.max(quarter, before?.quarter ?? 0) };
    if (!before) return;
    const next: string[] = [];
    if (going && quarter > before.quarter)
      next.push(t("janitorRun.announce.progress", { done, total }));
    if (before.going && state === "completed") {
      const parts = countParts(
        {
          containers: counts.removedContainers,
          networks: counts.removedNetworks,
          images: counts.removedImages,
        },
        t,
      );
      next.push(
        parts.length > 0
          ? t("janitorRun.announce.completed", { parts: parts.join(", ") })
          : t("janitorRun.announce.completedNone"),
      );
    }
    if (next.length > 0) setSpoken((s) => [...s, ...next].slice(-50));
  }, [going, quarter, done, total, state, counts, t]);
  return (
    <div data-slot="run-announcer" aria-live="polite" className="sr-only">
      {spoken.map((message, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: a message is only ever added at the end
        <p key={i}>{message}</p>
      ))}
    </div>
  );
}

interface RunBodyProps {
  envId: string;
  envName: string;
  run: JanitorRun;
  stream: JanitorStreamState;
  search: RunSearch;
  onSearchChange: (patch: RunSearchPatch) => void;
}

// RunFilters is the Show choice (one of All, Removed, Kept, Problems) and the search by name.
function RunFilters({
  search,
  onChange,
}: {
  search: RunSearch;
  onChange: (patch: RunSearchPatch) => void;
}) {
  const t = useT();
  const labelId = useId();
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
      <SearchBox
        value={search.q}
        onCommit={(q) => onChange({ q })}
        label={t("janitorRun.search.label")}
        placeholder={t("janitorRun.search.placeholder")}
        clearLabel={t("janitorRun.search.clear")}
      />
      <div className="flex items-center gap-2">
        <Label id={labelId} className="text-sm text-muted-foreground">
          {t("janitorRun.filter.label")}
        </Label>
        <ToggleGroup
          type="single"
          role="group"
          variant="outline"
          size="sm"
          spacing={2}
          aria-labelledby={labelId}
          value={search.show ?? "all"}
          onValueChange={(v) => {
            // Choosing the chosen one again is not a choice.
            if (v) onChange({ show: v === "all" ? undefined : (v as Show) });
          }}
        >
          {SHOW_VALUES.map((v) => (
            <ToggleGroupItem
              key={v}
              value={v}
              className="aria-checked:border-ring aria-checked:text-ring max-md:h-11"
            >
              {t(`janitorRun.filter.${v}`)}
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
      </div>
    </div>
  );
}

function RunBody({ envId, envName, run, stream, search, onSearchChange }: RunBodyProps) {
  const t = useT();
  const items = run.items ?? [];
  const counts = run.counts;
  const removed = countParts(
    {
      containers: counts.removedContainers,
      networks: counts.removedNetworks,
      images: counts.removedImages,
    },
    t,
  );
  const removedTotal = counts.removedContainers + counts.removedNetworks + counts.removedImages;
  const started = formatAbsolute(run.startedAt);
  const reasonWords = run.reason ? t(`janitorRun.reason.${run.reason}`) : "";
  const attempted = items.length;

  return (
    <div className="mx-auto flex w-full max-w-7xl flex-col gap-4">
      <Crumb envId={envId} label={t("janitorRun.breadcrumb.run", { time: started })} />
      <RunHeader envName={envName} run={run} chip={chipOf(run, stream)} />
      <RunProgress run={run} stream={stream} />
      <RunAnnouncer run={run} stream={stream} />

      {run.state === "waiting" ? (
        <Alert variant="warning" role="status">
          <Lock aria-hidden="true" />
          <AlertDescription className="text-foreground">{t("janitorRun.waiting")}</AlertDescription>
        </Alert>
      ) : null}

      <p className="text-sm">
        {removed.length > 0
          ? t("janitorRun.summary.removed", {
              parts: [
                ...removed,
                ...(run.estBytes === null
                  ? []
                  : [t("janitor.counts.size", { size: formatSize(run.estBytes, t) })]),
              ].join(", "),
            })
          : t("janitorRun.summary.removedNone")}{" "}
        {t("janitorRun.summary.kept", { count: counts.kept })}{" "}
        {t("janitorRun.summary.failed", { count: counts.failed })}
      </p>
      <p className="text-sm">
        <span className="text-xs font-semibold text-muted-foreground">
          {t("janitorRun.summary.space")}
        </span>{" "}
        <span className="tabular-nums">
          {run.estBytes === null
            ? t("janitorRun.summary.spaceUnknown")
            : t("janitor.counts.size", { size: formatSize(run.estBytes, t) })}
        </span>
      </p>

      {run.state === "partial" ? (
        <Alert variant="warning" role="status">
          <TriangleAlert aria-hidden="true" />
          <AlertTitle>{t("janitorRun.alert.partial.title")}</AlertTitle>
          <AlertDescription className="text-foreground">
            {t("janitorRun.alert.partial.body", {
              removed: removedTotal,
              total: attempted,
              failed: counts.failed,
              kept: counts.kept,
            })}
          </AlertDescription>
        </Alert>
      ) : null}
      {run.state === "skipped" ? (
        <Alert role="status">
          <Info aria-hidden="true" />
          <AlertTitle>{t("janitorRun.alert.skipped.title")}</AlertTitle>
          {reasonWords ? (
            <AlertDescription className="text-foreground">{reasonWords}</AlertDescription>
          ) : null}
        </Alert>
      ) : null}
      {run.state === "failed" ? (
        <Alert variant="destructive" role="alert">
          <CircleX aria-hidden="true" />
          <AlertTitle>{t("janitorRun.alert.failed.title")}</AlertTitle>
          <AlertDescription>
            {reasonWords
              ? t("janitorRun.alert.failed.body", { reason: reasonWords })
              : t("janitorRun.alert.failed.bodyNoReason")}
          </AlertDescription>
        </Alert>
      ) : null}

      {items.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t("janitorRun.empty")}</p>
      ) : (
        <>
          <RunFilters search={search} onChange={onSearchChange} />
          <RunItemsTable
            envId={envId}
            runId={run.id}
            items={items}
            show={search.show ?? "all"}
            q={search.q}
            onClearFilters={() => onSearchChange({ show: undefined, q: undefined })}
          />
        </>
      )}
    </div>
  );
}

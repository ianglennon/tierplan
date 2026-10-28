// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { getRouteApi } from "@tanstack/react-router";
import { ChevronRight } from "lucide-react";
import { useId, useState } from "react";

import { type AuditEntry, auditListQuery } from "@/api/audit";
import { environmentsQuery } from "@/api/environments";
import { RelativeTime } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { TruncatedText } from "@/features/resources/resource-table";
import { useInstanceAdmin } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { formatAbsolute, formatAbsoluteUtc } from "@/lib/format";
import { cn } from "@/lib/utils";
import { ActorName } from "./actor-name";
import { FilterBar } from "./filter-bar";
import { actionLabel, DEFAULT_RANGE } from "./filters";

const route = getRouteApi("/_app/audit");

// The columns, in order, after the one that holds the expand button.
const COLUMNS = ["time", "user", "action", "target", "environment", "result", "ip"] as const;
// The expand column plus the seven above: the width a detail row spans.
const COLUMN_COUNT = COLUMNS.length + 1;

// Refusal reasons the log has a sentence for; any other reason is shown as recorded.
const KNOWN_REASONS: readonly string[] = ["environment_not_visible"];

// The detail keys a coalesced refusal carries (D-20). They are shown as labelled
// rows, so they are not repeated in the raw details list.
const COALESCED_KEYS: readonly string[] = ["coalesced", "count", "first", "last"];

const SKELETON_ROWS = 8;
const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";

const RESULT_STATE = {
  success: "online",
  failure: "offline",
  denied: "offline",
} as const;

// Result is the outcome as a badge: a status dot and its text, never colour
// alone (UI-SPEC). Failure and denied share the offline colour.
function Result({ result }: { result: AuditEntry["result"] }) {
  const t = useT();
  return (
    <Badge variant="outline" className="gap-1.5 px-2">
      <StatusDot state={RESULT_STATE[result]} label={t(`audit.results.${result}`)} />
    </Badge>
  );
}

// Actor shows who did it. A system principal such as system:janitor is mono; a
// failed sign-in by a name nobody has shows "Anonymous", and the name that was
// tried is in Target (D-39).
function Actor({ entry }: { entry: AuditEntry }) {
  const t = useT();
  if (entry.actorType === "anonymous") return <span>{t("audit.anonymous")}</span>;
  if (entry.actorType === "system") {
    return <TruncatedText mono>{entry.actor}</TruncatedText>;
  }
  return <ActorName name={entry.actor} deleted={entry.actorDeleted} />;
}

interface DetailProps {
  label: string;
  children: string;
  mono?: boolean;
  // The user agent and a long target wrap inside the panel, whatever their
  // length, and never widen the table (UI-SPEC long text).
  wrap?: boolean;
}

// coalescedRefusal reads the burst a coalesced entry stands for: how many refusals
// came after the first, and when the first and the last were. It is undefined for
// every other entry. The server writes these as text, so a value that does not
// parse leaves the entry an ordinary one.
function coalescedRefusal(
  entry: AuditEntry,
): { count: string; first: string; last: string } | undefined {
  const d = entry.details ?? {};
  if (d.coalesced !== "true" || !/^\d+$/.test(d.count ?? "")) return undefined;
  return { count: d.count ?? "", first: d.first ?? entry.time, last: d.last ?? entry.time };
}

// TimeDetail is a time in the browser's zone with the same moment in UTC behind
// a tooltip, reachable by keyboard.
function TimeDetail({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex min-w-0 flex-col gap-0.5">
      <dt className="text-xs font-semibold text-muted-foreground">{label}</dt>
      <dd className="min-w-0 text-sm">
        <Tooltip>
          <TooltipTrigger asChild>
            <time
              dateTime={value}
              // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus (UI-SPEC D-66)
              tabIndex={0}
              className="rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
            >
              {formatAbsolute(value)}
            </time>
          </TooltipTrigger>
          <TooltipContent>{formatAbsoluteUtc(value)}</TooltipContent>
        </Tooltip>
      </dd>
    </div>
  );
}

function Detail({ label, children, mono, wrap }: DetailProps) {
  const t = useT();
  return (
    <div className="flex min-w-0 flex-col gap-0.5">
      <dt className="text-xs font-semibold text-muted-foreground">{label}</dt>
      <dd
        className={cn(
          "min-w-0 text-sm",
          mono && "font-mono text-xs",
          wrap && "break-all whitespace-pre-wrap",
        )}
      >
        {children === "" ? t("audit.none") : children}
      </dd>
    </div>
  );
}

// EntryDetails is what an expanded row shows (D-39): the time in UTC and in
// the browser's zone, the raw action key, the full target, user agent, IP, the
// reason, any extra details, and the entry ID. Everything is rendered as text.
function EntryDetails({ entry }: { entry: AuditEntry }) {
  const t = useT();
  const coalesced = coalescedRefusal(entry);
  const extra = Object.entries(entry.details ?? {})
    .filter(([key]) => !(coalesced && COALESCED_KEYS.includes(key)))
    .sort(([a], [b]) => a.localeCompare(b));
  const reason = KNOWN_REASONS.includes(entry.reason)
    ? t(`audit.reasons.${entry.reason}`)
    : entry.reason;
  return (
    // w-0 min-w-full: the panel takes the width the other rows gave the table
    // and adds none of its own, so a long user agent wraps instead of widening
    // the table.
    <div className="w-0 min-w-full bg-muted/30 px-3 py-3">
      <dl className="grid grid-cols-1 gap-x-6 gap-y-3 md:grid-cols-2">
        <Detail label={t("audit.details.utc")}>{formatAbsoluteUtc(entry.time)}</Detail>
        <Detail label={t("audit.details.local")}>{formatAbsolute(entry.time)}</Detail>
        <Detail label={t("audit.details.actionKey")} mono>
          {entry.action}
        </Detail>
        <Detail label={t("audit.details.target")} wrap>
          {entry.targetType ? `${entry.targetType}: ${entry.target}` : entry.target}
        </Detail>
        <Detail label={t("audit.details.ip")} mono>
          {entry.ip}
        </Detail>
        <Detail label={t("audit.details.reason")} wrap>
          {reason}
        </Detail>
        {coalesced ? (
          <>
            <TimeDetail label={t("audit.coalesced.first")} value={coalesced.first} />
            <TimeDetail label={t("audit.coalesced.last")} value={coalesced.last} />
            <Detail label={t("audit.coalesced.count")}>{coalesced.count}</Detail>
          </>
        ) : null}
        <div className="md:col-span-2">
          <Detail label={t("audit.details.userAgent")} wrap>
            {entry.userAgent}
          </Detail>
        </div>
        {extra.length > 0 ? (
          <div className="flex min-w-0 flex-col gap-0.5 md:col-span-2">
            <dt className="text-xs font-semibold text-muted-foreground">
              {t("audit.details.extra")}
            </dt>
            <dd className="min-w-0 font-mono text-xs break-all whitespace-pre-wrap">
              {extra.map(([key, value]) => (
                <div key={key}>{`${key}: ${value}`}</div>
              ))}
            </dd>
          </div>
        ) : null}
        <div className="md:col-span-2">
          <Detail label={t("audit.details.id")} mono>
            {entry.id}
          </Detail>
        </div>
      </dl>
    </div>
  );
}

interface EntryRowProps {
  entry: AuditEntry;
  environmentName: string | undefined;
}

// EntryRow is one log entry and its detail panel. Each entry is its own tbody,
// so the Collapsible can wrap the summary row and the panel row together.
function EntryRow({ entry, environmentName }: EntryRowProps) {
  const t = useT();
  const panelId = useId();
  const [open, setOpen] = useState(false);
  const label = actionLabel(entry.action, t, entry.details);
  const none = t("audit.none");
  const coalesced = coalescedRefusal(entry);
  const environment = entry.environment ? (environmentName ?? entry.environment) : none;

  return (
    <Collapsible asChild open={open} onOpenChange={setOpen}>
      <tbody data-slot="table-body" className="border-b last:border-0">
        <TableRow className={cn(ROW, "border-b-0")}>
          <TableCell className="w-10 px-1 py-0">
            <CollapsibleTrigger asChild>
              <Button
                variant="ghost"
                size="icon-sm"
                aria-controls={open ? panelId : undefined}
                aria-label={t(open ? "audit.details.hide" : "audit.details.show", {
                  action: label,
                })}
                className="max-md:size-11 [&[data-state=open]>svg]:rotate-90"
              >
                <ChevronRight aria-hidden="true" className="transition-transform" />
              </Button>
            </CollapsibleTrigger>
          </TableCell>
          <TableCell className={cn(CELL, "whitespace-nowrap")}>
            <RelativeTime value={coalesced ? coalesced.first : entry.time} />
          </TableCell>
          <TableCell className={CELL}>
            <Actor entry={entry} />
          </TableCell>
          <TableCell className={cn(CELL, "whitespace-nowrap")}>
            {coalesced ? (
              <span className="inline-flex items-center gap-2">
                {label}
                <Badge variant="outline" className="px-2 text-xs tabular-nums">
                  {t("audit.coalesced.badge", { count: coalesced.count })}
                </Badge>
              </span>
            ) : (
              label
            )}
          </TableCell>
          <TableCell className={CELL}>
            {entry.target ? <TruncatedText>{entry.target}</TruncatedText> : none}
          </TableCell>
          <TableCell className={cn(CELL, "whitespace-nowrap")}>
            {entry.environment ? <TruncatedText>{environment}</TruncatedText> : none}
          </TableCell>
          <TableCell className={cn(CELL, "whitespace-nowrap")}>
            <Result result={entry.result} />
          </TableCell>
          <TableCell className={cn(CELL, "font-mono text-xs whitespace-nowrap")}>
            {entry.ip || none}
          </TableCell>
        </TableRow>
        <CollapsibleContent asChild>
          <tr id={panelId} data-slot="table-row">
            <td colSpan={COLUMN_COUNT} className="p-0">
              <EntryDetails entry={entry} />
            </td>
          </tr>
        </CollapsibleContent>
      </tbody>
    </Collapsible>
  );
}

// AuditLog is the /audit screen (D-43): the retention and integrity caption,
// the filter bar, and a newest-first table that loads 50 entries at a time
// from a cursor. The filters are the URL's search params, so a filtered view is
// linkable and survives a refresh.
export function AuditLog() {
  const t = useT();
  const search = route.useSearch();
  const navigate = route.useNavigate();
  const query = useInfiniteQuery(auditListQuery(search));
  const environments = useQuery(environmentsQuery);
  // Anyone who is not an instance admin reads a scoped log (D-09).
  const scoped = !useInstanceAdmin();

  function change(patch: Partial<typeof search>) {
    void navigate({ search: (prev) => ({ ...prev, ...patch }), replace: true });
  }
  function clear() {
    void navigate({ search: { range: DEFAULT_RANGE }, replace: true });
  }

  const pages = query.data?.pages ?? [];
  const entries = pages.flatMap((p) => p.items ?? []);
  const retentionDays = pages[0]?.retentionDays;
  const loading = query.isPending;
  const failed = query.isError && pages.length === 0;
  const empty = !loading && !failed && entries.length === 0;
  const envNames = new Map((environments.data?.items ?? []).map((e) => [e.id, e.name]));

  return (
    <div className="mx-auto flex w-full max-w-7xl flex-col gap-4">
      <header className="flex flex-col gap-1">
        <h1 className="text-2xl font-semibold">{t("audit.title")}</h1>
        <p className="text-sm text-muted-foreground">
          {retentionDays === undefined
            ? null
            : retentionDays === 0
              ? t("audit.retentionForever")
              : t("audit.retention", { count: retentionDays })}{" "}
          {t("audit.integrity.before")}{" "}
          <code className="rounded bg-muted px-1 py-0.5 font-mono text-xs text-foreground">
            {t("audit.integrity.command")}
          </code>{" "}
          {t("audit.integrity.after")}
        </p>
        {scoped ? (
          <p className="text-sm text-muted-foreground">{t("audit.scopedCaption")}</p>
        ) : null}
      </header>

      <FilterBar search={search} onChange={change} onClear={clear} />

      {failed ? (
        <Alert variant="destructive">
          <AlertTitle>{t("audit.error.title")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("audit.error.body")}</span>
            <Button
              variant="outline"
              size="sm"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {query.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("audit.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}

      {empty ? (
        <Empty className="min-h-[30svh] border">
          <EmptyHeader>
            <EmptyTitle role="heading" aria-level={2} className="text-base">
              {t("audit.empty.title")}
            </EmptyTitle>
            <EmptyDescription>{t("audit.empty.body")}</EmptyDescription>
          </EmptyHeader>
          <EmptyContent>
            <Button variant="outline" onClick={clear}>
              {t("audit.empty.clear")}
            </Button>
          </EmptyContent>
        </Empty>
      ) : null}

      {!failed && !empty ? (
        // Scrolls sideways on a phone (D-64) and down past the page height, so
        // the sticky header has something to stick to.
        <div className="relative max-h-[calc(100dvh-18rem)] overflow-x-auto overflow-y-auto rounded-xl ring-1 ring-foreground/10">
          <table
            data-slot="table"
            aria-busy={loading || undefined}
            aria-label={t("audit.title")}
            className="w-full caption-bottom text-sm"
          >
            <TableHeader>
              <TableRow className={cn(ROW, "hover:bg-transparent")}>
                <TableHead className="sticky top-0 z-10 w-10 bg-muted px-1 max-md:h-11">
                  <span className="sr-only">{t("audit.columns.details")}</span>
                </TableHead>
                {COLUMNS.map((c) => (
                  <TableHead
                    key={c}
                    className="sticky top-0 z-10 h-9 bg-muted px-3 text-xs font-semibold text-muted-foreground max-md:h-11"
                  >
                    {t(`audit.columns.${c}`)}
                  </TableHead>
                ))}
              </TableRow>
            </TableHeader>
            {loading ? (
              <TableBody>
                {Array.from({ length: SKELETON_ROWS }, (_, i) => (
                  // Placeholder rows have no identity of their own.
                  // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows
                  <TableRow key={i} className={ROW}>
                    <TableCell className="w-10 px-1 py-0" />
                    {COLUMNS.map((c) => (
                      <TableCell key={c} className={CELL}>
                        <Skeleton className="h-4 w-24" aria-hidden="true" />
                      </TableCell>
                    ))}
                  </TableRow>
                ))}
              </TableBody>
            ) : (
              entries.map((entry) => (
                <EntryRow
                  key={entry.id}
                  entry={entry}
                  environmentName={entry.environment ? envNames.get(entry.environment) : undefined}
                />
              ))
            )}
          </table>
        </div>
      ) : null}

      {query.isFetchNextPageError ? (
        <Alert variant="destructive">
          <AlertTitle>{t("audit.error.title")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("audit.error.body")}</span>
            <Button variant="outline" size="sm" onClick={() => void query.fetchNextPage()}>
              {t("audit.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}

      {query.hasNextPage && !query.isFetchNextPageError ? (
        <div className="flex justify-center">
          <Button
            variant="outline"
            disabled={query.isFetchingNextPage}
            onClick={() => void query.fetchNextPage()}
            className="max-md:h-11"
          >
            {query.isFetchingNextPage ? <Spinner aria-hidden="true" /> : null}
            {t("audit.loadMore")}
          </Button>
        </div>
      ) : null}
    </div>
  );
}

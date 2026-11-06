// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { RefreshCw, Search, TriangleAlert, X } from "lucide-react";
import { useEffect, useId, useRef, useState } from "react";
import { Trans } from "react-i18next";

import { environmentsQuery } from "@/api/environments";
import {
  clampQuery,
  SEARCH_MAX,
  SEARCH_TYPES,
  type SearchResult,
  type SearchType,
  searchable,
  useSearch,
} from "@/api/search";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { EnvFilter } from "@/features/fleet/env-filter";
import { VirtualRows } from "@/features/fleet/virtual-rows";
import { STATUS_CHIP_ROW } from "@/features/resources/status-chips";
import { MatchText } from "@/features/search/match-text";
import { hitKey, hitTarget, ICONS, useDetail } from "@/features/search/search-hits";
import type { SearchPageSearch } from "@/features/search/search-params";
import { searchTiming, useDebounced } from "@/features/search/use-debounced";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// Past this many rows the table draws only the rows in view (UI-SPEC "Fleet tables"). The server
// answers with at most 200.
const VIRTUALISE_OVER = 100;
const ROW_PX = 44;
const COLUMNS = 4;
const SKELETON_ROWS = 6;
const CELL = "px-3 py-0";

interface SearchResultsPageProps {
  // The address: the query, the type chip and the environments, already validated.
  search: SearchPageSearch;
  onSearchChange: (patch: Partial<SearchPageSearch>) => void;
}

// SearchResultsPage is the full page behind the palette's "Search all results" (D-18): the same
// search, every match instead of five of each kind, narrowed by type and environment in the URL.
// Every name, image and environment is placed as text.
export function SearchResultsPage({ search, onSearchChange }: SearchResultsPageProps) {
  const t = useT();
  useDocumentTitle(t("search.page.title"));
  const inputId = useId();
  const input = useRef<HTMLInputElement>(null);

  const { q = "", type, env = [] } = search;

  // The text box is the person's typing; the address follows 200 ms after they pause. A query that
  // arrives from outside (the palette, a link, Back) replaces the text, but the echo of what this
  // box just wrote does not, or a character typed in between would be lost.
  const [text, setText] = useState(q);
  const written = useRef(q);
  const latest = useRef({ q, onSearchChange });
  latest.current = { q, onSearchChange };
  useEffect(() => {
    if (q !== written.current) {
      written.current = q;
      setText(q);
    }
  }, [q]);
  const settled = useDebounced(text, searchTiming.debounceMs);
  useEffect(() => {
    const next = clampQuery(settled.trim()).trim();
    if (next === latest.current.q) return;
    written.current = next;
    latest.current.onSearchChange({ q: next === "" ? undefined : next });
  }, [settled]);

  const searching = searchable(q);
  const environments = useQuery(environmentsQuery);
  const items = environments.data?.items ?? [];

  // The chips count every type, so the counts come from the search over all types; the rows come
  // from the search for the chosen type. With the chip on All they are the same request.
  const all = useSearch(q, "page", "all", env);
  const rows = useSearch(q, "page", type ?? "all", env);
  const head = all.data ?? rows.data;
  const failed = searching && (all.isError || rows.isError) && !all.isFetching && !rows.isFetching;
  const waiting = searching && !failed && rows.data === undefined;
  const results = rows.data?.results ?? [];
  const busy = all.isFetching || rows.isFetching;

  const notSearched = head?.notSearched ?? [];
  const scope = env.length > 0 ? env.length : items.length;
  const searched = Math.max(scope - notSearched.length, 0);

  const again = () => {
    void all.refetch();
    if (type) void rows.refetch();
  };

  // The new count after a chip or an environment filter changes, read once the answer is in.
  const announce = useDebounced(
    searching && rows.data && !busy
      ? t("search.page.announce", { count: rows.data.results?.length ?? 0 })
      : "",
    searchTiming.announceMs,
  );

  const clear = () => {
    setText("");
    input.current?.focus();
  };

  const chipLabel = (value: "all" | SearchType) => {
    const label = value === "all" ? t("search.page.chips.all") : t(`search.type.${value}.heading`);
    return head ? t("search.page.chips.counted", { label, count: head.counts[value] }) : label;
  };

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-4">
      <header className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <h1 className="text-2xl font-semibold">{t("search.page.title")}</h1>
        {searching && head && environments.data ? (
          <span className="text-xs text-muted-foreground tabular-nums">
            <Trans
              i18nKey="search.page.caption"
              ns="search"
              count={head.counts.all}
              values={{ environments: t("search.page.environments", { count: searched }) }}
              components={{ time: <RelativeTime value={head.searchedAt} /> }}
            />
          </span>
        ) : null}
        {searching ? (
          <Button
            variant="ghost"
            size="sm"
            className="ms-auto max-md:h-11"
            aria-busy={busy || undefined}
            onClick={() => {
              if (!busy) again();
            }}
          >
            {busy ? <Spinner aria-hidden="true" /> : <RefreshCw aria-hidden="true" />}
            {t("search.page.again")}
          </Button>
        ) : null}
      </header>

      <InputGroup className="max-w-xl max-md:h-11">
        <InputGroupAddon>
          <Search aria-hidden="true" />
        </InputGroupAddon>
        <InputGroupInput
          ref={input}
          id={inputId}
          type="text"
          autoComplete="off"
          spellCheck={false}
          aria-label={t("search.page.input.label")}
          placeholder={t("search.page.input.placeholder")}
          value={text}
          onChange={(e) => setText(clampQuery(e.target.value))}
          maxLength={SEARCH_MAX * 2}
        />
        {text !== "" ? (
          <InputGroupAddon align="inline-end">
            <InputGroupButton
              size="icon-xs"
              aria-label={t("search.page.input.clear")}
              onClick={clear}
              className="max-md:size-9"
            >
              <X aria-hidden="true" />
            </InputGroupButton>
          </InputGroupAddon>
        ) : null}
      </InputGroup>

      {searching ? (
        <div className="flex flex-wrap items-center gap-2">
          <ToggleGroup
            type="single"
            variant="outline"
            size="sm"
            value={type ?? "all"}
            aria-label={t("search.page.chips.label")}
            className={STATUS_CHIP_ROW}
            onValueChange={(next) => {
              // Pressing the chip that is on turns it off; one chip is always on.
              if (next === "all") onSearchChange({ type: undefined });
              else {
                const known = SEARCH_TYPES.find((s) => s === next);
                if (known) onSearchChange({ type: known });
              }
            }}
          >
            {(["all", ...SEARCH_TYPES] as const).map((value) => (
              <ToggleGroupItem key={value} value={value} className="max-md:h-11">
                {chipLabel(value)}
              </ToggleGroupItem>
            ))}
          </ToggleGroup>
          <EnvFilter
            value={env}
            onChange={(ids) => onSearchChange({ env: ids.length > 0 ? ids : undefined })}
            environments={items.map((e) => ({ id: e.id, name: e.name }))}
          />
        </div>
      ) : null}

      <div role="status" className="sr-only">
        {announce}
      </div>

      {searching && notSearched.length > 0 ? (
        <Alert variant="warning" role="status">
          <TriangleAlert aria-hidden="true" />
          <AlertTitle>{t("search.page.notSearched.title")}</AlertTitle>
          <AlertDescription className="break-words">
            {t("search.page.notSearched.body", {
              names: notSearched.map((n) => n.envName).join(", "),
            })}
          </AlertDescription>
        </Alert>
      ) : null}

      {!searching ? (
        <Empty>
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <Search aria-hidden="true" />
            </EmptyMedia>
            <EmptyTitle>{t("search.page.short.title")}</EmptyTitle>
            <EmptyDescription>{t("search.page.short.body")}</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : null}

      {failed ? (
        <Alert variant="destructive" role="alert">
          <AlertTitle>{t("search.error.title")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("search.error.body")}</span>
            <Button variant="outline" size="sm" className="max-md:h-11" onClick={again}>
              {t("search.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}

      {searching && !failed && !waiting && results.length === 0 ? (
        <Empty>
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <Search aria-hidden="true" />
            </EmptyMedia>
            <EmptyTitle>{t("search.page.none.title", { q })}</EmptyTitle>
            <EmptyDescription>{t("search.page.none.body", { count: searched })}</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : null}

      {searching && !failed && (waiting || results.length > 0) ? (
        <ResultRows results={results} loading={waiting} q={q} />
      ) : null}

      {rows.data?.truncated ? (
        <p className="text-xs text-muted-foreground tabular-nums">
          {t("search.page.truncated", { count: results.length })}
        </p>
      ) : null}
    </div>
  );
}

interface ResultRowsProps {
  results: readonly SearchResult[];
  loading: boolean;
  q: string;
}

function ResultRows({ results, loading, q }: ResultRowsProps) {
  const t = useT();
  const [scroller, setScroller] = useState<HTMLDivElement | null>(null);
  const virtual = results.length > VIRTUALISE_OVER;
  const renderRow = (hit: SearchResult) => <ResultRow key={hitKey(hit)} hit={hit} q={q} />;

  return (
    <div
      ref={setScroller}
      className={cn(
        "rounded-lg border",
        virtual ? "max-h-[70svh] overflow-auto" : "overflow-x-auto",
      )}
    >
      <table
        className="w-full caption-bottom text-sm"
        aria-busy={loading || undefined}
        aria-label={loading ? t("search.page.table.loading") : undefined}
      >
        <caption className="sr-only">{t("search.page.table.caption")}</caption>
        <TableHeader className={virtual ? "sticky top-0 z-10 bg-background" : undefined}>
          <TableRow className="h-12 hover:bg-transparent">
            <TableHead scope="col" className="w-36">
              {t("search.page.columns.type")}
            </TableHead>
            <TableHead scope="col" className="min-w-56">
              {t("search.page.columns.name")}
            </TableHead>
            <TableHead scope="col" className="w-48">
              {t("search.page.columns.environment")}
            </TableHead>
            <TableHead scope="col" className="min-w-64">
              {t("search.page.columns.details")}
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {loading ? (
            Array.from({ length: SKELETON_ROWS }, (_, i) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: placeholder rows have no identity
              <TableRow key={i} className="h-11" aria-hidden="true">
                {Array.from({ length: COLUMNS }, (_, c) => (
                  // biome-ignore lint/suspicious/noArrayIndexKey: placeholder cells have no identity
                  <TableCell key={c} className={CELL}>
                    <Skeleton className="h-4 w-4/5" />
                  </TableCell>
                ))}
              </TableRow>
            ))
          ) : virtual ? (
            <VirtualRows
              rows={results}
              scroller={scroller}
              columns={COLUMNS}
              rowHeight={ROW_PX}
              rowKey={hitKey}
              renderRow={renderRow}
            />
          ) : (
            results.map(renderRow)
          )}
        </TableBody>
      </table>
    </div>
  );
}

const LINK =
  "rounded-sm hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none";

function ResultRow({ hit, q }: { hit: SearchResult; q: string }) {
  const t = useT();
  const detail = useDetail();
  const target = hitTarget(hit);
  const Icon = ICONS[hit.type];
  return (
    <TableRow className="h-11">
      <TableCell className={cn(CELL, "w-36 whitespace-nowrap")}>
        <span className="inline-flex items-center gap-2">
          <Icon className="text-muted-foreground" aria-hidden />
          {t(`search.page.typeWord.${hit.type}`)}
        </span>
      </TableCell>
      <TableCell className={cn(CELL, "min-w-56")}>
        <Link
          to={target.to}
          params={target.params}
          search={target.search}
          title={hit.name}
          className={cn(LINK, "block max-w-96 truncate font-mono")}
        >
          <MatchText text={hit.name} q={q} />
        </Link>
      </TableCell>
      <TableCell className={cn(CELL, "w-48")}>
        <Link
          to="/env/$envId"
          params={{ envId: hit.envId }}
          title={hit.envName}
          className={cn(LINK, "block max-w-44 truncate")}
        >
          {hit.envName}
        </Link>
      </TableCell>
      <TableCell className={cn(CELL, "min-w-64 py-1")}>
        <span className="line-clamp-2 text-xs break-words text-muted-foreground">
          {detail(hit)}
        </span>
      </TableCell>
    </TableRow>
  );
}

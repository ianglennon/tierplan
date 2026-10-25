// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ReactNode } from "react";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { SearchBox } from "@/features/containers/container-list";
import {
  STACK_STATUS_VALUES,
  type StackKindValue,
  type StackStatusValue,
  stackStatusList,
  stackStatusParam,
} from "@/features/stacks/list-search";
import { useT } from "@/i18n/use-t";

// How many stacks each chip counts, as the server answers them: before the search and the chips, so the
// numbers never shrink with what was typed.
export interface StackStatusCounts {
  degraded: number;
  stopped: number;
  draft: number;
  deployFailed: number;
  drifted: number;
}

// How many stacks the Show group counts, as the server answers them: before the search, the chips and the
// kind itself.
export interface StackKindCounts {
  all: number;
  managed: number;
  unmanaged: number;
}

const COUNT_KEY: Record<StackStatusValue, keyof StackStatusCounts> = {
  degraded: "degraded",
  stopped: "stopped",
  draft: "draft",
  deploy_failed: "deployFailed",
  drifted: "drifted",
};

interface StackFilterBarProps {
  q: string | undefined;
  status: string | undefined;
  // Undefined while the first answer is on its way: the chips show their words without numbers.
  counts: StackStatusCounts | undefined;
  // The Show group: the kind in the URL (absent is both), the numbers it counts and the way to change it.
  kind: StackKindValue | undefined;
  kindCounts: StackKindCounts | undefined;
  onKind: (kind: StackKindValue | undefined) => void;
  // True when a search, a chip or, on the All stacks list, an environment is on: Clear filters shows.
  filtered: boolean;
  onSearch: (q: string | undefined) => void;
  onStatus: (status: string | undefined) => void;
  onClear: () => void;
  // The class of the chip row: STATUS_CHIP_ROW, so the chips wrap at every width (the services list's).
  chipRowClassName: string;
  // The controls of the first row after the search (the All stacks list's Environment and Group by).
  children?: ReactNode;
}

// StackFilterBar is the toolbar both stack lists share (UI-SPEC "Stack lists"): row 1 the search (250 ms
// after the last key, replacing the history entry), the Show group, the list's own controls and Clear filters
// while a filter is on; row 2 the status chips with their counts, wrapping at every width. The chips filter
// the stacks Bayplan manages only.
export function StackFilterBar({
  q,
  status,
  counts,
  kind,
  kindCounts,
  onKind,
  filtered,
  onSearch,
  onStatus,
  onClear,
  chipRowClassName,
  children,
}: StackFilterBarProps) {
  const t = useT();
  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2 max-md:[&>*]:min-w-0">
        <SearchBox
          value={q}
          onCommit={onSearch}
          label={t("stackLists.search.label")}
          placeholder={t("stackLists.search.placeholder")}
          clearLabel={t("stackLists.search.clear")}
        />
        <ShowGroup kind={kind} counts={kindCounts} onKind={onKind} />
        {children}
        {filtered ? (
          <Button variant="ghost" className="max-md:h-11" onClick={onClear}>
            {t("stackLists.clearFilters")}
          </Button>
        ) : null}
      </div>
      <ToggleGroup
        type="multiple"
        // The chips are toggle buttons in a labelled group, not a toolbar.
        role="group"
        variant="outline"
        size="sm"
        spacing={2}
        aria-label={t("stackLists.status.label")}
        value={stackStatusList(status)}
        onValueChange={(v) => onStatus(stackStatusParam(v as StackStatusValue[]))}
        className={chipRowClassName}
      >
        {STACK_STATUS_VALUES.map((v) => {
          const label = t(`stackLists.status.${v}`);
          return (
            <ToggleGroupItem
              key={v}
              value={v}
              className="aria-pressed:border-ring aria-pressed:text-ring max-md:h-11"
            >
              {counts ? t("stackLists.status.chip", { label, count: counts[COUNT_KEY[v]] }) : label}
            </ToggleGroupItem>
          );
        })}
      </ToggleGroup>
    </div>
  );
}

const SHOW_VALUES = ["all", "managed", "unmanaged"] as const;

interface ShowGroupProps {
  kind: StackKindValue | undefined;
  counts: StackKindCounts | undefined;
  onKind: (kind: StackKindValue | undefined) => void;
}

// ShowGroup is the Managed / Unmanaged filter: a labelled single-select group with All, Managed and
// Unmanaged, each with its count in its accessible name ("Unmanaged, 3"). Pressing the one that is on turns
// it off, which is All.
function ShowGroup({ kind, counts, onKind }: ShowGroupProps) {
  const t = useT();
  return (
    <ToggleGroup
      type="single"
      role="group"
      variant="outline"
      size="sm"
      spacing={2}
      aria-label={t("stackUnmanaged.show.label")}
      value={kind ?? "all"}
      onValueChange={(v) => onKind(v === "managed" || v === "unmanaged" ? v : undefined)}
      className="max-md:flex-wrap"
    >
      {SHOW_VALUES.map((v) => {
        const label = t(`stackUnmanaged.show.${v}`);
        const n = counts?.[v];
        return (
          <ToggleGroupItem
            key={v}
            value={v}
            aria-label={n === undefined ? label : t(`stackUnmanaged.show.${v}`, { count: n })}
            className="aria-checked:border-ring aria-checked:text-ring max-md:h-11"
          >
            {label}
            {n === undefined ? null : (
              <span className="ms-1 text-muted-foreground tabular-nums">{n}</span>
            )}
          </ToggleGroupItem>
        );
      })}
    </ToggleGroup>
  );
}

interface DiscoveryFailedNoticeProps {
  // The environment whose look for unmanaged stacks failed.
  name: string;
  onRetry: () => void;
  retrying: boolean;
}

// DiscoveryFailedNotice is the one-line warning row for an environment whose look for unmanaged stacks
// failed: its managed stacks are listed all the same.
export function DiscoveryFailedNotice({ name, onRetry, retrying }: DiscoveryFailedNoticeProps) {
  const t = useT();
  return (
    <Alert role="status">
      <AlertDescription className="flex flex-wrap items-center gap-3">
        <span>{t("stackUnmanaged.discovery.failed", { name })}</span>
        <Button variant="outline" size="sm" disabled={retrying} onClick={onRetry}>
          {t("stackUnmanaged.discovery.retry")}
        </Button>
      </AlertDescription>
    </Alert>
  );
}

interface StackKindEmptyProps {
  // The kind that lets nothing through.
  kind: StackKindValue;
  // The environment's name on its own list; undefined on the All stacks list.
  envName?: string;
  onClear: () => void;
}

// StackKindEmpty is the empty state of a list that the Show group alone leaves with nothing: every stack is
// managed, or every stack was created elsewhere.
export function StackKindEmpty({ kind, envName, onClear }: StackKindEmptyProps) {
  const t = useT();
  return (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base">
          {t(`stackUnmanaged.empty.${kind}.title`)}
        </EmptyTitle>
        <EmptyDescription className="break-words">
          {envName === undefined
            ? t(`stackUnmanaged.empty.${kind}.bodyGlobal`)
            : t(`stackUnmanaged.empty.${kind}.body`, { name: envName })}
        </EmptyDescription>
      </EmptyHeader>
      <EmptyContent>
        <Button variant="outline" className="max-md:h-11" onClick={onClear}>
          {t("stackUnmanaged.empty.action")}
        </Button>
      </EmptyContent>
    </Empty>
  );
}

interface StackNoMatchProps {
  // The search text, when it is the only thing on: its empty state names it.
  q: string | undefined;
  // True when a chip or an environment is on as well as, or instead of, a search.
  moreThanSearch: boolean;
  onClearSearch: () => void;
  onClear: () => void;
}

// StackNoMatch is the empty state of a list that has stacks but none that the search or the filters
// let through. A search alone says so and offers Clear search; a chip or an environment may be the
// cause, so those say the filters match nothing and offer Clear filters (UI-SPEC "Core elements").
export function StackNoMatch({ q, moreThanSearch, onClearSearch, onClear }: StackNoMatchProps) {
  const t = useT();
  const searchOnly = Boolean(q) && !moreThanSearch;
  return (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base break-words">
          {searchOnly
            ? t("stackLists.emptySearch.title", { q })
            : t("stackLists.emptyFilters.title")}
        </EmptyTitle>
      </EmptyHeader>
      <EmptyContent>
        <Button
          variant="outline"
          className="max-md:h-11"
          onClick={searchOnly ? onClearSearch : onClear}
        >
          {searchOnly ? t("stackLists.emptySearch.action") : t("stackLists.emptyFilters.action")}
        </Button>
      </EmptyContent>
    </Empty>
  );
}

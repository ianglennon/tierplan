// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ReactNode } from "react";

import { Button } from "@/components/ui/button";
import { Empty, EmptyContent, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { SearchBox } from "@/features/containers/container-list";
import {
  STACK_STATUS_VALUES,
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
}

const COUNT_KEY: Record<StackStatusValue, keyof StackStatusCounts> = {
  degraded: "degraded",
  stopped: "stopped",
  draft: "draft",
  deploy_failed: "deployFailed",
};

interface StackFilterBarProps {
  q: string | undefined;
  status: string | undefined;
  // Undefined while the first answer is on its way: the chips show their words without numbers.
  counts: StackStatusCounts | undefined;
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
// after the last key, replacing the history entry), the list's own controls and Clear filters while a
// filter is on; row 2 the status chips with their counts, wrapping at every width.
export function StackFilterBar({
  q,
  status,
  counts,
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

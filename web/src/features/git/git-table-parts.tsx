// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { type ReactNode, useEffect, useId, useMemo, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { SearchBox } from "@/features/containers/container-list";
import type { RowState } from "@/features/resources/resource-table";
import { useMe } from "@/hooks/use-me";
import { needsOf } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// The Scope filter's value for "every scope". The Select cannot hold an empty value, and the URL
// carries no scope at all for it.
const ALL_SCOPES = "all";
export const INSTANCE_SCOPE = "instance";

// The environments the Scope filter offers.
export interface ScopeOption {
  id: string;
  name: string;
}

// What the two tables filter by: the search text and the Scope ("instance" or an environment's ID).
export interface GitFilters {
  q?: string;
  scope?: string;
}

export type GitFiltersPatch = { [K in keyof GitFilters]?: GitFilters[K] | undefined };

// byText orders as a person reads: case does not matter, then the exact text so the order never
// depends on how the rows arrived.
export function byText(a: string, b: string): number {
  const x = a.toLowerCase();
  const y = b.toLowerCase();
  if (x !== y) return x < y ? -1 : 1;
  return a === b ? 0 : a < b ? -1 : 1;
}

// The scope of a row, as the Scope filter names it.
export interface RowScope {
  kind: "instance" | "environment";
  envId: string | null;
  envName: string | null;
}

export function inScope(scope: RowScope, filter: string | undefined): boolean {
  if (filter === undefined) return true;
  if (filter === INSTANCE_SCOPE) return scope.kind === "instance";
  return scope.envId === filter;
}

// byScopeThenName is the default order (UI-SPEC "Git page"): instance rows first, then each
// environment's by environment name, then by name.
export function byScopeThenName(
  a: { scope: RowScope; name: string },
  b: { scope: RowScope; name: string },
): number {
  const instance = Number(b.scope.kind === "instance") - Number(a.scope.kind === "instance");
  if (instance !== 0) return instance;
  return byText(a.scope.envName ?? "", b.scope.envName ?? "") || byText(a.name, b.name);
}

type Dot = "online" | "warning" | "offline";

const DOT_CLASS: Record<Dot, string> = {
  online: "bg-status-online",
  warning: "bg-status-warning",
  offline: "bg-status-offline",
};
const DOT_TEXT: Record<Dot, string> = {
  online: "text-status-online-text",
  warning: "text-status-warning-text",
  offline: "text-status-offline-text",
};

// A coloured dot with its words beside it: the state is never colour alone.
export function DotText({ dot, children }: { dot: Dot; children: ReactNode }) {
  return (
    <span className={cn("inline-flex items-center gap-1.5 text-xs", DOT_TEXT[dot])}>
      <span
        aria-hidden="true"
        className={cn("inline-block size-2 shrink-0 rounded-full", DOT_CLASS[dot])}
      />
      {children}
    </span>
  );
}

// useManageReason says why the signed-in user cannot change a row, in the words of the
// disabled-with-reason pattern: "Needs Admin on {environment name}" for an environment's row (the
// role comes from the requirements /api/me lists, never from here) and "Needs an instance
// administrator" for the instance's. It is undefined for a row they can change.
export function useManageReason(): (row: {
  readOnly: boolean;
  scope: RowScope;
}) => string | undefined {
  const t = useT();
  const me = useMe();
  return (row) => {
    if (!row.readOnly) return undefined;
    if (row.scope.kind === "instance" || !row.scope.envId) return t("git.needs.instance");
    const need = needsOf(me.data, "git.manage", row.scope.envId, t).reason ?? t("needs.role.admin");
    return t("git.needs.onEnvironment", { need, name: row.scope.envName ?? row.scope.envId });
  };
}

interface GitToolbarProps {
  filters: GitFilters;
  onFiltersChange: (patch: GitFiltersPatch) => void;
  scopes: readonly ScopeOption[];
  search: { label: string; placeholder: string; clear: string };
  // Whether any filter is on, which shows Clear filters.
  filtered: boolean;
}

// GitToolbar is the search box, the Scope select and Clear filters above both tables.
export function GitToolbar({
  filters,
  onFiltersChange,
  scopes,
  search,
  filtered,
}: GitToolbarProps) {
  const t = useT();
  const id = useId();
  const scopeValue = filters.scope ?? ALL_SCOPES;
  // A scope in the URL that is no longer listed stays selectable, so the Select never shows a blank.
  const known =
    filters.scope === undefined ||
    filters.scope === INSTANCE_SCOPE ||
    scopes.some((s) => s.id === filters.scope);
  return (
    <div className="flex flex-wrap items-end gap-3">
      <SearchBox
        value={filters.q}
        onCommit={(q) => onFiltersChange({ q })}
        label={search.label}
        placeholder={search.placeholder}
        clearLabel={search.clear}
      />
      <div className="flex flex-col gap-1">
        <Label htmlFor={`${id}-scope`} className="text-xs text-muted-foreground">
          {t("git.toolbar.scope.label")}
        </Label>
        <Select
          value={scopeValue}
          onValueChange={(next) =>
            onFiltersChange({ scope: next === ALL_SCOPES ? undefined : next })
          }
        >
          <SelectTrigger id={`${id}-scope`} className="w-48 max-md:h-11">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={ALL_SCOPES}>{t("git.toolbar.scope.all")}</SelectItem>
            <SelectItem value={INSTANCE_SCOPE}>{t("git.toolbar.scope.instance")}</SelectItem>
            {scopes.map((s) => (
              <SelectItem key={s.id} value={s.id}>
                {s.name}
              </SelectItem>
            ))}
            {known ? null : (
              <SelectItem value={filters.scope as string}>{filters.scope}</SelectItem>
            )}
          </SelectContent>
        </Select>
      </div>
      {filtered ? (
        <Button
          variant="ghost"
          className="max-md:h-11"
          onClick={() => onFiltersChange({ q: undefined, scope: undefined })}
        >
          {t("git.toolbar.clear")}
        </Button>
      ) : null}
    </div>
  );
}

interface UsedByCountProps {
  // How many rows use this one (the server's count).
  count: number;
  // The plural-aware text of the count: "{n} stacks" or "{n} repositories".
  label: string;
  // The names of up to all users the page knows of; the popover lists the first 10.
  names: readonly string[];
  // The row's own name, for the popover's accessible name.
  rowName: string;
}

const POPOVER_NAMES = 10;

// UsedByCount is the "Used by" cell: a plain "0" when nothing uses the row, otherwise a
// link-styled count that opens a popover of up to 10 names and then "and {n} more". Names are
// typed by people and are drawn as text only.
export function UsedByCount({ count, label, names, rowName }: UsedByCountProps) {
  const t = useT();
  if (count === 0) return <span className="text-xs tabular-nums">{label}</span>;
  const shown = names.slice(0, POPOVER_NAMES);
  const more = Math.max(count - shown.length, 0);
  return (
    <Popover>
      <PopoverTrigger asChild>
        <button
          type="button"
          aria-label={t("git.usedBy.popover", { name: rowName })}
          className="rounded-sm text-start text-xs tabular-nums text-accent-ink underline-offset-3 hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11"
        >
          {label}
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-64">
        <ul className="flex flex-col gap-1 text-sm">
          {shown.map((n) => (
            <li key={n} className="break-all">
              {n}
            </li>
          ))}
          {more > 0 ? (
            <li className="text-muted-foreground">{t("git.usedBy.more", { count: more })}</li>
          ) : null}
        </ul>
      </PopoverContent>
    </Popover>
  );
}

// How long a row that appeared stays muted (UI-SPEC "Row change highlight").
export const APPEARED_MS = 1500;

// useAppearedRows says which rows appeared while the page was open: the ones in the first list
// that arrives are not new, and a row that shows up in a later answer is muted for 1.5 s, as the
// other lists do, so a person sees what the refresh brought. ids is undefined while loading.
export function useAppearedRows(ids: readonly string[] | undefined): ReadonlyMap<string, RowState> {
  const seen = useRef<Set<string> | null>(null);
  const timers = useRef(new Set<ReturnType<typeof setTimeout>>());
  const [fresh, setFresh] = useState<ReadonlySet<string>>(() => new Set());

  useEffect(() => {
    if (!ids) return;
    if (seen.current === null) {
      seen.current = new Set(ids);
      return;
    }
    const known = seen.current;
    const appeared = ids.filter((id) => !known.has(id));
    if (appeared.length === 0) return;
    for (const id of appeared) known.add(id);
    setFresh((prev) => new Set([...prev, ...appeared]));
    const timer = setTimeout(() => {
      timers.current.delete(timer);
      setFresh((prev) => {
        const next = new Set(prev);
        for (const id of appeared) next.delete(id);
        return next;
      });
    }, APPEARED_MS);
    timers.current.add(timer);
  }, [ids]);

  useEffect(() => {
    const live = timers.current;
    return () => {
      for (const timer of live) clearTimeout(timer);
      live.clear();
    };
  }, []);

  return useMemo(() => new Map([...fresh].map((id) => [id, { changed: true }])), [fresh]);
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Ellipsis, Plus } from "lucide-react";
import { type ReactNode, useMemo, useState } from "react";

import type { Credential } from "@/api/registries";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime, useNow } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { SearchBox } from "@/features/containers/container-list";
import { CredentialBadge } from "@/features/registries/credential-badge";
import {
  type Column,
  ResourceTable,
  type TableSort,
  TruncatedText,
} from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// The Scope filter's value for "every scope". The Select cannot hold an empty value,
// and the URL carries no scope at all for it.
const ALL_SCOPES = "all";
export const INSTANCE_SCOPE = "instance";

// A warning shows this long before the end of the last day a credential is valid.
const WARN_DAYS = 7;
const DAY_MS = 86_400_000;

// The environments the Scope filter offers, as {id, name}.
export interface ScopeOption {
  id: string;
  name: string;
}

export interface CredentialFilters {
  q?: string;
  // "instance" or an environment's ID; absent for every scope.
  scope?: string;
}

// byText orders as a person reads: case does not matter, then the exact text so the
// order never depends on how the rows arrived.
function byText(a: string, b: string): number {
  const x = a.toLowerCase();
  const y = b.toLowerCase();
  if (x !== y) return x < y ? -1 : 1;
  return a === b ? 0 : a < b ? -1 : 1;
}

const registryOf = (c: Credential) => (c.prefix ? `${c.host}/${c.prefix}` : c.host);

// The default order (UI-SPEC "Registries page"): instance credentials first, then by
// registry host, then prefix. An environment's own credentials follow, by environment.
export function byDefaultOrder(a: Credential, b: Credential): number {
  const instance = Number(b.scope.kind === "instance") - Number(a.scope.kind === "instance");
  if (instance !== 0) return instance;
  return (
    byText(a.scope.envName ?? "", b.scope.envName ?? "") ||
    byText(a.host, b.host) ||
    byText(a.prefix, b.prefix) ||
    byText(a.name, b.name)
  );
}

type SortKey = "name" | "registry" | "expires";

// compareBy orders by a sortable column; the default order breaks ties.
function compareBy(key: SortKey, dir: "asc" | "desc"): (a: Credential, b: Credential) => number {
  const sign = dir === "asc" ? 1 : -1;
  return (a, b) => {
    let result = 0;
    if (key === "name") result = byText(a.name, b.name);
    else if (key === "registry") result = byText(registryOf(a), registryOf(b));
    else {
      // No expiry sorts last in either direction, so the dated ones stay together.
      if (a.expiresAt === null || b.expiresAt === null) {
        if (a.expiresAt === b.expiresAt) return byDefaultOrder(a, b);
        return a.expiresAt === null ? 1 : -1;
      }
      result = Date.parse(a.expiresAt) - Date.parse(b.expiresAt);
    }
    return result !== 0 ? sign * result : byDefaultOrder(a, b);
  };
}

// matches is the search: the text is found in the name, the registry or the username.
function matches(c: Credential, q: string): boolean {
  const needle = q.toLowerCase();
  return [c.name, registryOf(c), c.username].some((v) => v.toLowerCase().includes(needle));
}

function inScope(c: Credential, scope: string | undefined): boolean {
  if (scope === undefined) return true;
  if (scope === INSTANCE_SCOPE) return c.scope.kind === "instance";
  return c.scope.envId === scope;
}

// filterCredentials applies the search and the Scope filter, in the default order.
export function filterCredentials(rows: readonly Credential[], filters: CredentialFilters) {
  return rows
    .filter((c) => inScope(c, filters.scope) && (!filters.q || matches(c, filters.q)))
    .sort(byDefaultOrder);
}

// maskOf is the Secret cell: four dots and the last characters the server kept, or the
// dots alone when it kept none (a short secret).
export function maskOf(c: Credential): string {
  return `••••${c.last4}`;
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
function DotText({ dot, children }: { dot: Dot; children: ReactNode }) {
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

export function ExpiresCell({ expiresAt }: { expiresAt: string | null }) {
  const t = useT();
  const now = useNow();
  if (expiresAt === null) return <span>{t("dashboard.time.missing")}</span>;
  const left = Date.parse(expiresAt) - now;
  if (left <= 0) return <DotText dot="offline">{t("registries.table.expired")}</DotText>;
  const days = Math.ceil(left / DAY_MS);
  if (days <= WARN_DAYS) {
    return <DotText dot="warning">{t("registries.table.expiresIn", { count: days })}</DotText>;
  }
  return <RelativeTime value={expiresAt} />;
}

export function LastTestCell({ test }: { test: Credential["lastTest"] }) {
  const t = useT();
  if (test === null) {
    return <span className="text-xs text-muted-foreground">{t("registries.table.notTested")}</span>;
  }
  const cell = (
    <DotText dot={test.ok ? "online" : "offline"}>
      <span>{t(test.ok ? "registries.table.testPassed" : "registries.table.testFailed")}</span>
      <RelativeTime value={test.at} />
    </DotText>
  );
  // The failure reason is a short sentence the server wrote, shown as text.
  if (test.ok || !test.detail) return cell;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span>{cell}</span>
      </TooltipTrigger>
      <TooltipContent className="max-w-sm">{test.detail}</TooltipContent>
    </Tooltip>
  );
}

function LastUseCell({ failure }: { failure: Credential["lastFailure"] }) {
  const t = useT();
  if (failure === null) return <span>{t("dashboard.time.missing")}</span>;
  return (
    <span className="inline-flex items-center gap-1 text-xs text-status-offline-text">
      <span>{t("registries.table.signInFailed")}</span>
      <RelativeTime value={failure.at} />
    </span>
  );
}

interface CredentialTableProps {
  // The credentials the caller may see, or undefined while they load.
  credentials: readonly Credential[] | undefined;
  isPending: boolean;
  isError: boolean;
  isFetching: boolean;
  refetch: () => unknown;
  filters: CredentialFilters;
  onFiltersChange: (
    patch: { [K in keyof CredentialFilters]?: CredentialFilters[K] | undefined },
  ) => void;
  // The environments the Scope filter lists.
  scopes: readonly ScopeOption[];
  // Whether the caller may add an instance credential (registries.instance).
  canAdd: boolean;
  onAdd: () => void;
  // Opens the credential's sheet.
  onOpen: (id: string) => void;
  // Asks to delete a credential the caller may change.
  onDelete: (credential: Credential) => void;
}

// CredentialTable is the Registries page's table (IMG-01, D-09): every stored
// credential the caller may see, the secret only as its last characters. A
// credential the caller may not change offers "View details" alone; one they may change
// offers Edit, a separator, then Delete…. Names, hosts,
// prefixes and usernames are typed by people and drawn as text only.
export function CredentialTable({
  credentials,
  isPending,
  isError,
  isFetching,
  refetch,
  filters,
  onFiltersChange,
  scopes,
  canAdd,
  onAdd,
  onOpen,
  onDelete,
}: CredentialTableProps) {
  const t = useT();
  const [sorted, setSorted] = useState<{ key: SortKey; dir: "asc" | "desc" } | null>(null);

  const rows = useMemo(() => {
    const found = filterCredentials(credentials ?? [], filters);
    return sorted ? [...found].sort(compareBy(sorted.key, sorted.dir)) : found;
  }, [credentials, filters, sorted]);
  const filtered = Boolean(filters.q) || filters.scope !== undefined;
  const loaded = credentials !== undefined;
  const total = credentials?.length ?? 0;

  const sort: TableSort = {
    key: sorted?.key ?? "",
    dir: sorted?.dir ?? "asc",
    onSort: (key) =>
      setSorted((current) =>
        current?.key === key
          ? { key: current.key, dir: current.dir === "asc" ? "desc" : "asc" }
          : { key: key as SortKey, dir: "asc" },
      ),
  };

  const columns: Column<Credential>[] = [
    {
      id: "name",
      header: t("registries.table.columns.name"),
      sortKey: "name",
      className: "min-w-40",
      cell: (c) => (
        <Tooltip>
          <TooltipTrigger asChild>
            <button
              type="button"
              onClick={() => onOpen(c.id)}
              className="block max-w-64 truncate rounded-sm text-start font-medium text-accent-ink underline-offset-3 hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11"
            >
              {c.name}
            </button>
          </TooltipTrigger>
          <TooltipContent>{c.name}</TooltipContent>
        </Tooltip>
      ),
    },
    {
      id: "scope",
      header: t("registries.table.columns.scope"),
      className: "w-40 min-w-40",
      cell: (c) => <CredentialBadge scope={c.scope} />,
    },
    {
      id: "type",
      header: t("registries.table.columns.type"),
      className: "w-30 min-w-30",
      cell: (c) => t(`registries.type.${c.type}`),
    },
    {
      id: "registry",
      header: t("registries.table.columns.registry"),
      sortKey: "registry",
      className: "min-w-56",
      cell: (c) => (
        <span className="flex items-center gap-2">
          <TruncatedText mono>{registryOf(c)}</TruncatedText>
          {c.hubFallback ? (
            <Badge variant="outline" className="shrink-0">
              {t("registries.table.hubFallback")}
            </Badge>
          ) : null}
        </span>
      ),
    },
    {
      id: "username",
      header: t("registries.table.columns.username"),
      className: "w-40 min-w-40",
      cell: (c) => <TruncatedText>{c.username}</TruncatedText>,
    },
    {
      id: "secret",
      header: t("registries.table.columns.secret"),
      className: "w-28 min-w-28",
      cell: (c) => <span className="font-mono text-xs">{maskOf(c)}</span>,
    },
    {
      id: "expires",
      header: t("registries.table.columns.expires"),
      sortKey: "expires",
      className: "w-36 min-w-36",
      cell: (c) => <ExpiresCell expiresAt={c.expiresAt} />,
    },
    {
      id: "lastTest",
      header: t("registries.table.columns.lastTest"),
      className: "w-44 min-w-44",
      cell: (c) => <LastTestCell test={c.lastTest} />,
    },
    {
      id: "lastUse",
      header: t("registries.table.columns.lastUse"),
      className: "w-44 min-w-44",
      cell: (c) => <LastUseCell failure={c.lastFailure} />,
    },
    {
      id: "actions",
      header: <span className="sr-only">{t("registries.table.columns.actions")}</span>,
      className: "w-11 min-w-11 px-0 text-center",
      cell: (c) => (
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button
              variant="ghost"
              size="icon-sm"
              className="max-md:size-11"
              aria-label={t("registries.table.actionsFor", { name: c.name })}
            >
              <Ellipsis aria-hidden="true" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            {c.canManage ? (
              <>
                <DropdownMenuItem className="max-md:min-h-11" onSelect={() => onOpen(c.id)}>
                  {t("registries.table.edit")}
                </DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem
                  className="max-md:min-h-11"
                  variant="destructive"
                  onSelect={() => onDelete(c)}
                >
                  {t("registries.table.delete")}
                </DropdownMenuItem>
              </>
            ) : (
              <DropdownMenuItem className="max-md:min-h-11" onSelect={() => onOpen(c.id)}>
                {t("registries.table.viewDetails")}
              </DropdownMenuItem>
            )}
          </DropdownMenuContent>
        </DropdownMenu>
      ),
    },
  ];

  const scopeValue = filters.scope ?? ALL_SCOPES;
  // A scope in the URL that is no longer listed stays selectable, so the Select never
  // shows a blank.
  const known =
    filters.scope === undefined ||
    filters.scope === INSTANCE_SCOPE ||
    scopes.some((s) => s.id === filters.scope);

  const toolbar = (
    <div className="flex flex-wrap items-end gap-3">
      <SearchBox
        value={filters.q}
        onCommit={(q) => onFiltersChange({ q })}
        label={t("registries.toolbar.search.label")}
        placeholder={t("registries.toolbar.search.placeholder")}
        clearLabel={t("registries.toolbar.search.clear")}
      />
      <div className="flex flex-col gap-1">
        <Label htmlFor="registries-scope" className="text-xs text-muted-foreground">
          {t("registries.toolbar.scope.label")}
        </Label>
        <Select
          value={scopeValue}
          onValueChange={(next) =>
            onFiltersChange({ scope: next === ALL_SCOPES ? undefined : next })
          }
        >
          <SelectTrigger id="registries-scope" className="w-48 max-md:h-11">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={ALL_SCOPES}>{t("registries.toolbar.scope.all")}</SelectItem>
            <SelectItem value={INSTANCE_SCOPE}>{t("registries.toolbar.scope.instance")}</SelectItem>
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
          {t("registries.toolbar.clear")}
        </Button>
      ) : null}
    </div>
  );

  const addButton = (
    <NeedsTooltip reason={canAdd ? undefined : t("registries.needsInstance")}>
      <Button
        className="ms-auto max-md:order-last max-md:h-11 max-md:w-full"
        onClick={canAdd ? onAdd : undefined}
      >
        <Plus aria-hidden="true" />
        {t("registries.add")}
      </Button>
    </NeedsTooltip>
  );

  const empty = filtered ? (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base">
          {t("registries.empty.filtered.title")}
        </EmptyTitle>
      </EmptyHeader>
      <EmptyContent>
        <Button
          variant="outline"
          className="max-md:h-11"
          onClick={() => onFiltersChange({ q: undefined, scope: undefined })}
        >
          {t("registries.empty.filtered.action")}
        </Button>
      </EmptyContent>
    </Empty>
  ) : (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base">
          {t(canAdd ? "registries.empty.manage.title" : "registries.empty.view.title")}
        </EmptyTitle>
        <EmptyDescription>
          {t(canAdd ? "registries.empty.manage.body" : "registries.empty.view.body")}
        </EmptyDescription>
      </EmptyHeader>
      {canAdd ? (
        <EmptyContent>
          <Button className="max-md:h-11" onClick={onAdd}>
            <Plus aria-hidden="true" />
            {t("registries.add")}
          </Button>
        </EmptyContent>
      ) : null}
    </Empty>
  );

  const caption = filtered
    ? t("registries.table.countOf", { count: rows.length, total })
    : t("registries.table.count", { count: rows.length });

  return (
    <ResourceTable<Credential>
      resource="registries"
      i18nKey="registries.table"
      errorBody={t("registries.table.errorBody")}
      columns={columns}
      query={{
        data: loaded ? { items: rows, total: rows.length, truncated: false } : undefined,
        isPending,
        isError,
        isFetching,
        refetch,
      }}
      envName=""
      getRowKey={(c) => c.id}
      caption={caption}
      sort={sort}
      actions={addButton}
      banner={
        <p className="max-w-prose text-sm text-muted-foreground">{t("registries.caption")}</p>
      }
      toolbar={toolbar}
      empty={empty}
    />
  );
}

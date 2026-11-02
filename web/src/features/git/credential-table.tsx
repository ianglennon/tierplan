// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Ellipsis, Plus } from "lucide-react";
import { type ReactNode, useId, useMemo, useState } from "react";

import type { GitCredential } from "@/api/git";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
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
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  byScopeThenName,
  byText,
  type GitFilters,
  type GitFiltersPatch,
  GitToolbar,
  inScope,
  type ScopeOption,
  UsedByCount,
  useManageReason,
} from "@/features/git/git-table-parts";
import { CredentialBadge } from "@/features/registries/credential-badge";
import {
  type Column,
  ResourceTable,
  type RowState,
  type TableSort,
  TruncatedText,
} from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";

type SortKey = "name" | "lastUse";

// maskOf is the Secret cell of a token: four dots and the last characters the server kept, or the
// dots alone when it kept none (a short token).
export function maskOf(c: Pick<GitCredential, "last4">): string {
  return `••••${c.last4}`;
}

// matches is the search: the text is found in the name or the username.
function matches(c: GitCredential, q: string): boolean {
  const needle = q.toLowerCase();
  return [c.name, c.username].some((v) => v.toLowerCase().includes(needle));
}

// filterCredentials applies the search and the Scope filter, in the default order: instance rows
// first, then by name.
export function filterCredentials(
  rows: readonly GitCredential[],
  filters: GitFilters,
): GitCredential[] {
  return rows
    .filter((c) => inScope(c.scope, filters.scope) && (!filters.q || matches(c, filters.q)))
    .sort(byScopeThenName);
}

// The newest sign-in the table can speak of: when the git host last refused the credential, or the
// last time a test signed in with it.
function lastUseOf(c: GitCredential): number | null {
  const at = c.lastFailure?.at ?? c.lastUseAt;
  return at ? Date.parse(at) : null;
}

function compareBy(
  key: SortKey,
  dir: "asc" | "desc",
): (a: GitCredential, b: GitCredential) => number {
  const sign = dir === "asc" ? 1 : -1;
  return (a, b) => {
    let result = 0;
    if (key === "name") result = byText(a.name, b.name);
    else {
      // A credential never used sorts last in either direction.
      const x = lastUseOf(a);
      const y = lastUseOf(b);
      if (x === null || y === null) {
        if (x === y) return byScopeThenName(a, b);
        return x === null ? 1 : -1;
      }
      result = x - y;
    }
    return result !== 0 ? sign * result : byScopeThenName(a, b);
  };
}

function LastUseCell({ credential: c }: { credential: GitCredential }) {
  const t = useT();
  if (c.lastFailure !== null) {
    return (
      <span className="inline-flex items-center gap-1 text-xs text-status-offline-text">
        <span>{t("git.credentials.refused")}</span>
        <RelativeTime value={c.lastFailure.at} />
      </span>
    );
  }
  if (c.lastUseAt === null) return <span>{t("dashboard.time.missing")}</span>;
  return <RelativeTime value={c.lastUseAt} />;
}

interface CredentialTableProps {
  credentials: readonly GitCredential[] | undefined;
  isPending: boolean;
  isError: boolean;
  isFetching: boolean;
  refetch: () => unknown;
  filters: GitFilters;
  onFiltersChange: (patch: GitFiltersPatch) => void;
  scopes: readonly ScopeOption[];
  // Whether the caller may add a credential at all (git.instance, or git.manage on an environment).
  canAdd: boolean;
  // The page's caption and tabs, drawn under the title.
  banner: ReactNode;
  onAdd: () => void;
  onOpen: (id: string) => void;
  onDelete: (credential: GitCredential) => void;
  // The names of the repositories that use each credential, by credential ID.
  usedByNames: ReadonlyMap<string, readonly string[]>;
  // A row that appeared while the page was open, muted for 1.5 s.
  rowStates?: ReadonlyMap<string, RowState>;
}

// CredentialTable is the Credentials tab (GIT-01, D-01, D-02): every credential the caller may see.
// The secret is never here, only the last characters of a token or the fingerprint of a deploy
// key's public half. Names and usernames are typed by people and drawn as text only. A row the
// caller may only see offers "View details" alone, and says why it cannot be changed.
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
  banner,
  onAdd,
  onOpen,
  onDelete,
  usedByNames,
  rowStates,
}: CredentialTableProps) {
  const t = useT();
  const reasonOf = useManageReason();
  const reasonIds = useId();
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

  const columns: Column<GitCredential>[] = [
    {
      id: "name",
      header: t("git.credentials.columns.name"),
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
      header: t("git.credentials.columns.scope"),
      className: "w-40 min-w-40",
      cell: (c) => <CredentialBadge scope={c.scope} />,
    },
    {
      id: "type",
      header: t("git.credentials.columns.type"),
      className: "w-36 min-w-36",
      cell: (c) => t(`git.credentials.type.${c.type}`),
    },
    {
      id: "identity",
      header: t("git.credentials.columns.identity"),
      className: "min-w-48",
      // A token shows the user name it is sent with; a deploy key the fingerprint of its public key.
      cell: (c) => (
        <TruncatedText mono>{c.type === "ssh" ? c.fingerprint : c.username}</TruncatedText>
      ),
    },
    {
      id: "secret",
      header: t("git.credentials.columns.secret"),
      className: "w-36 min-w-36",
      cell: (c) =>
        c.type === "ssh" ? (
          <span className="text-xs">{t("git.credentials.privateKeySaved")}</span>
        ) : (
          <span className="font-mono text-xs">{maskOf(c)}</span>
        ),
    },
    {
      id: "usedBy",
      header: t("git.credentials.columns.usedBy"),
      className: "w-36 min-w-36",
      cell: (c) => (
        <UsedByCount
          count={c.usedBy}
          label={t("git.usedBy.repositories", { count: c.usedBy })}
          names={usedByNames.get(c.id) ?? []}
          rowName={c.name}
        />
      ),
    },
    {
      id: "lastUse",
      header: t("git.credentials.columns.lastUse"),
      sortKey: "lastUse",
      className: "w-40 min-w-40",
      cell: (c) => <LastUseCell credential={c} />,
    },
    {
      id: "actions",
      header: <span className="sr-only">{t("git.credentials.columns.actions")}</span>,
      className: "w-11 min-w-11 px-0 text-center",
      cell: (c) => {
        const reason = reasonOf(c);
        const reasonId = `${reasonIds}-${c.id}`;
        const inUse =
          c.usedBy > 0 ? t("git.usedBy.disabledCredential", { count: c.usedBy }) : undefined;
        return (
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button
                variant="ghost"
                size="icon-sm"
                className="max-md:size-11"
                aria-label={t("git.credentials.actionsFor", { name: c.name })}
                aria-describedby={reason ? reasonId : undefined}
              >
                <Ellipsis aria-hidden="true" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              {!c.readOnly ? (
                <>
                  <DropdownMenuItem className="max-md:min-h-11" onSelect={() => onOpen(c.id)}>
                    {t("git.credentials.edit")}
                  </DropdownMenuItem>
                  <DropdownMenuSeparator />
                  <NeedsTooltip reason={inUse}>
                    <DropdownMenuItem
                      className="max-md:min-h-11"
                      variant="destructive"
                      onSelect={() => onDelete(c)}
                    >
                      {t("git.credentials.delete")}
                    </DropdownMenuItem>
                  </NeedsTooltip>
                </>
              ) : (
                <DropdownMenuItem className="max-md:min-h-11" onSelect={() => onOpen(c.id)}>
                  {t("git.credentials.viewDetails")}
                </DropdownMenuItem>
              )}
            </DropdownMenuContent>
            {reason ? (
              <span id={reasonId} className="sr-only">
                {reason}
              </span>
            ) : null}
          </DropdownMenu>
        );
      },
    },
  ];

  // Without any right to add, the button is not drawn at all (D-50: hide what a person cannot open).
  const addButton = canAdd ? (
    <Button className="ms-auto max-md:order-last max-md:h-11 max-md:w-full" onClick={onAdd}>
      <Plus aria-hidden="true" />
      {t("git.credentials.add")}
    </Button>
  ) : undefined;

  const clear = () => onFiltersChange({ q: undefined, scope: undefined });
  const empty = filtered ? (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base">
          {filters.q
            ? t("git.credentials.empty.filtered.title", { query: filters.q })
            : t("git.credentials.empty.filtered.titleScope")}
        </EmptyTitle>
      </EmptyHeader>
      <EmptyContent>
        <Button variant="outline" className="max-md:h-11" onClick={clear}>
          {t("git.credentials.empty.filtered.action")}
        </Button>
      </EmptyContent>
    </Empty>
  ) : (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base">
          {t(canAdd ? "git.credentials.empty.manage.title" : "git.credentials.empty.view.title")}
        </EmptyTitle>
        <EmptyDescription>
          {t(canAdd ? "git.credentials.empty.manage.body" : "git.credentials.empty.view.body")}
        </EmptyDescription>
      </EmptyHeader>
      {canAdd ? (
        <EmptyContent>
          <Button className="max-md:h-11" onClick={onAdd}>
            <Plus aria-hidden="true" />
            {t("git.credentials.add")}
          </Button>
        </EmptyContent>
      ) : null}
    </Empty>
  );

  const caption = filtered
    ? t("git.credentials.countOf", { count: rows.length, total })
    : t("git.credentials.count", { count: rows.length });

  return (
    <ResourceTable<GitCredential>
      resource="git"
      i18nKey="git.credentials"
      title={t("git.title")}
      tableLabel={t("git.credentials.title")}
      errorBody={t("git.credentials.errorBody")}
      retryLabel={t("git.credentials.retry")}
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
      rowStates={rowStates}
      banner={banner}
      toolbar={
        <GitToolbar
          filters={filters}
          onFiltersChange={onFiltersChange}
          scopes={scopes}
          filtered={filtered}
          search={{
            label: t("git.credentials.search.label"),
            placeholder: t("git.credentials.search.placeholder"),
            clear: t("git.credentials.search.clear"),
          }}
        />
      }
      empty={empty}
    />
  );
}

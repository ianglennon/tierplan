// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { CircleX, Ellipsis, Plus, ShieldCheck, TriangleAlert } from "lucide-react";
import { useId, useMemo, useState } from "react";

import type { GitRepo } from "@/api/git";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
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
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { addressKind, hasBuiltinHostKey, withoutUserinfo } from "@/features/git/address-rules";
import {
  byScopeThenName,
  byText,
  DotText,
  type GitFilters,
  type GitFiltersPatch,
  GitToolbar,
  inScope,
  type ScopeOption,
  UsedByCount,
  useManageReason,
} from "@/features/git/git-table-parts";
import { useShortReason } from "@/features/git/repo-test-result";
import { CredentialBadge } from "@/features/registries/credential-badge";
import {
  type Column,
  ResourceTable,
  type RowState,
  type TableSort,
  TruncatedText,
} from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";

type SortKey = "name" | "lastTest";

// The host key states a repository's Host key cell shows.
export type HostKeyState = "none" | "builtin" | "trusted" | "untrusted" | "changed";

// hostKeyState is what the Host key cell says about a repository, from the list alone: an https
// address has no host key, a host with built-in keys needs none, and any other ssh host is
// "Trusted" with the key a person confirmed, "Not trusted yet" without one, or "Changed" when the
// last test stopped at a key that is not the trusted one.
export function hostKeyState(repo: GitRepo): HostKeyState {
  if (addressKind(repo.url) !== "ssh") return "none";
  if (hasBuiltinHostKey(repo.url)) return "builtin";
  if (repo.lastTest && !repo.lastTest.ok && repo.lastTest.code === "host_key_changed") {
    return "changed";
  }
  return repo.hostKey ? "trusted" : "untrusted";
}

function HostKeyCell({ repo }: { repo: GitRepo }) {
  const t = useT();
  const state = hostKeyState(repo);
  if (state === "none") {
    return <span className="text-xs text-muted-foreground">{t("git.repos.hostKey.none")}</span>;
  }
  const label = t(`git.repos.hostKey.${state}`);
  if (state === "builtin") {
    return (
      <span className="inline-flex items-center gap-1.5 text-xs">
        <ShieldCheck aria-hidden="true" className="size-4 text-status-online-text" />
        {label}
      </span>
    );
  }
  if (state === "trusted") {
    const fingerprint = repo.hostKey?.fingerprint ?? "";
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="inline-flex items-center gap-1.5 text-xs">
            <ShieldCheck aria-hidden="true" className="size-4 text-status-online-text" />
            {label}
            <span className="font-mono">{fingerprint.slice(0, 12)}</span>
          </span>
        </TooltipTrigger>
        <TooltipContent className="max-w-sm break-all font-mono">{fingerprint}</TooltipContent>
      </Tooltip>
    );
  }
  if (state === "changed") {
    return (
      <span className="inline-flex items-center gap-1.5 text-xs text-status-offline-text">
        <CircleX aria-hidden="true" className="size-4" />
        {label}
      </span>
    );
  }
  return (
    <span className="inline-flex items-center gap-1.5 text-xs text-status-warning-text">
      <TriangleAlert aria-hidden="true" className="size-4" />
      {label}
    </span>
  );
}

export function LastTestCell({ test }: { test: GitRepo["lastTest"] }) {
  const t = useT();
  const short = useShortReason();
  if (test === null) {
    return <span className="text-xs text-muted-foreground">{t("git.repos.notTested")}</span>;
  }
  const cell = (
    <DotText dot={test.ok ? "online" : "offline"}>
      <span>{t(test.ok ? "git.repos.testPassed" : "git.repos.testFailed")}</span>
      <RelativeTime value={test.at} />
    </DotText>
  );
  if (test.ok || !test.code) return cell;
  // The failure reason is a code the server stored, worded here; a Tooltip is a second route to
  // it, never the only one (the cell is real text).
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span>{cell}</span>
      </TooltipTrigger>
      <TooltipContent className="max-w-sm">{short(test.code)}</TooltipContent>
    </Tooltip>
  );
}

// matches is the search: the text is found in the name or the address.
function matches(r: GitRepo, q: string): boolean {
  const needle = q.toLowerCase();
  return [r.name, r.url].some((v) => v.toLowerCase().includes(needle));
}

// filterRepos applies the search and the Scope filter, in the default order: instance rows
// first, then by name.
export function filterRepos(rows: readonly GitRepo[], filters: GitFilters): GitRepo[] {
  return rows
    .filter((r) => inScope(r.scope, filters.scope) && (!filters.q || matches(r, filters.q)))
    .sort(byScopeThenName);
}

function compareBy(key: SortKey, dir: "asc" | "desc"): (a: GitRepo, b: GitRepo) => number {
  const sign = dir === "asc" ? 1 : -1;
  return (a, b) => {
    let result = 0;
    if (key === "name") result = byText(a.name, b.name);
    else {
      // A repository never tested sorts last in either direction.
      if (a.lastTest === null || b.lastTest === null) {
        if (a.lastTest === b.lastTest) return byScopeThenName(a, b);
        return a.lastTest === null ? 1 : -1;
      }
      result = Date.parse(a.lastTest.at) - Date.parse(b.lastTest.at);
    }
    return result !== 0 ? sign * result : byScopeThenName(a, b);
  };
}

interface RepositoryTableProps {
  repos: readonly GitRepo[] | undefined;
  isPending: boolean;
  isError: boolean;
  isFetching: boolean;
  refetch: () => unknown;
  filters: GitFilters;
  onFiltersChange: (patch: GitFiltersPatch) => void;
  scopes: readonly ScopeOption[];
  // Whether the caller may add a repository at all (git.instance, or git.manage on an environment).
  canAdd: boolean;
  // The title and caption block above the table (the page's own).
  banner: React.ReactNode;
  onAdd: () => void;
  onOpen: (id: string) => void;
  // Opens a credential's sheet (the Credential cell is a link to it).
  onOpenCredential?: (id: string) => void;
  onTest: (repo: GitRepo) => void;
  // "Re-trust host key…": offered to those who may manage a row whose key changed or was never
  // trusted.
  onRetrust?: (repo: GitRepo) => void;
  onDelete: (repo: GitRepo) => void;
  // The ID of a row that appeared while the page was open, highlighted for 1.5 s.
  rowStates?: ReadonlyMap<string, RowState>;
  // Disables a row's Test while it runs.
  testingId?: string | null;
}

// RepositoryTable is the Repositories tab (GIT-01, D-01, D-02): every repository the caller may
// see, instance rows first. Names, addresses and credential names are typed by people and drawn as
// text only, with any user name or password in an address removed. A row the caller may only see
// offers "View details" alone, and says why it cannot be changed.
export function RepositoryTable({
  repos,
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
  onOpenCredential,
  onTest,
  onRetrust,
  onDelete,
  rowStates,
  testingId,
}: RepositoryTableProps) {
  const t = useT();
  const reasonOf = useManageReason();
  const reasonIds = useId();
  const [sorted, setSorted] = useState<{ key: SortKey; dir: "asc" | "desc" } | null>(null);

  const rows = useMemo(() => {
    const found = filterRepos(repos ?? [], filters);
    return sorted ? [...found].sort(compareBy(sorted.key, sorted.dir)) : found;
  }, [repos, filters, sorted]);
  const filtered = Boolean(filters.q) || filters.scope !== undefined;
  const loaded = repos !== undefined;
  const total = repos?.length ?? 0;

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

  const columns: Column<GitRepo>[] = [
    {
      id: "name",
      header: t("git.repos.columns.name"),
      sortKey: "name",
      className: "min-w-40",
      cell: (r) => (
        <Tooltip>
          <TooltipTrigger asChild>
            <button
              type="button"
              onClick={() => onOpen(r.id)}
              className="block max-w-64 truncate rounded-sm text-start font-medium text-accent-ink underline-offset-3 hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11"
            >
              {r.name}
            </button>
          </TooltipTrigger>
          <TooltipContent>{r.name}</TooltipContent>
        </Tooltip>
      ),
    },
    {
      id: "scope",
      header: t("git.repos.columns.scope"),
      className: "w-40 min-w-40",
      cell: (r) => <CredentialBadge scope={r.scope} />,
    },
    {
      id: "address",
      header: t("git.repos.columns.address"),
      className: "min-w-64",
      cell: (r) => (
        <span className="flex items-center gap-2">
          <TruncatedText mono className="max-w-80">
            {withoutUserinfo(r.url)}
          </TruncatedText>
          {r.hasCa ? (
            <Badge variant="outline" className="shrink-0">
              {t("git.badge.customCa")}
            </Badge>
          ) : null}
        </span>
      ),
    },
    {
      id: "credential",
      header: t("git.repos.columns.credential"),
      className: "w-44 min-w-44",
      cell: (r) => {
        if (r.credential === null) {
          return (
            <span className="text-xs text-muted-foreground">{t("git.repos.noCredential")}</span>
          );
        }
        const credential = r.credential;
        if (!onOpenCredential) return <TruncatedText>{credential.name}</TruncatedText>;
        return (
          <button
            type="button"
            onClick={() => onOpenCredential(credential.id)}
            className="block max-w-40 truncate rounded-sm text-start text-accent-ink underline-offset-3 hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11"
          >
            {credential.name}
          </button>
        );
      },
    },
    {
      id: "hostKey",
      header: t("git.repos.columns.hostKey"),
      className: "w-44 min-w-44",
      cell: (r) => <HostKeyCell repo={r} />,
    },
    {
      id: "lastTest",
      header: t("git.repos.columns.lastTest"),
      sortKey: "lastTest",
      className: "w-44 min-w-44",
      cell: (r) => <LastTestCell test={r.lastTest} />,
    },
    {
      id: "usedBy",
      header: t("git.repos.columns.usedBy"),
      className: "w-28 min-w-28",
      // Stacks cannot be listed by name until the stack API names them; the count is the server's.
      cell: (r) => (
        <UsedByCount
          count={r.usedBy}
          label={t("git.usedBy.stacks", { count: r.usedBy })}
          names={[]}
          rowName={r.name}
        />
      ),
    },
    {
      id: "actions",
      header: <span className="sr-only">{t("git.repos.columns.actions")}</span>,
      className: "w-11 min-w-11 px-0 text-center",
      cell: (r) => {
        const reason = reasonOf(r);
        const reasonId = `${reasonIds}-${r.id}`;
        const inUse =
          r.usedBy > 0 ? t("git.usedBy.disabledRepository", { count: r.usedBy }) : undefined;
        return (
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button
                variant="ghost"
                size="icon-sm"
                className="max-md:size-11"
                aria-label={t("git.repos.actionsFor", { name: r.name })}
                aria-describedby={reason ? reasonId : undefined}
              >
                <Ellipsis aria-hidden="true" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              {!r.readOnly ? (
                <>
                  <DropdownMenuItem className="max-md:min-h-11" onSelect={() => onOpen(r.id)}>
                    {t("git.repos.edit")}
                  </DropdownMenuItem>
                  <DropdownMenuItem
                    className="max-md:min-h-11"
                    disabled={testingId === r.id}
                    onSelect={() => onTest(r)}
                  >
                    {t("git.repos.test")}
                  </DropdownMenuItem>
                  {onRetrust &&
                  (hostKeyState(r) === "changed" || hostKeyState(r) === "untrusted") ? (
                    <DropdownMenuItem className="max-md:min-h-11" onSelect={() => onRetrust(r)}>
                      {t("git.repos.retrustHostKey")}
                    </DropdownMenuItem>
                  ) : null}
                  <DropdownMenuSeparator />
                  <NeedsTooltip reason={inUse}>
                    <DropdownMenuItem
                      className="max-md:min-h-11"
                      variant="destructive"
                      onSelect={() => onDelete(r)}
                    >
                      {t("git.repos.delete")}
                    </DropdownMenuItem>
                  </NeedsTooltip>
                </>
              ) : (
                <DropdownMenuItem className="max-md:min-h-11" onSelect={() => onOpen(r.id)}>
                  {t("git.repos.viewDetails")}
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
      {t("git.repos.add")}
    </Button>
  ) : undefined;

  const clear = () => onFiltersChange({ q: undefined, scope: undefined });
  const empty = filtered ? (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base">
          {filters.q
            ? t("git.repos.empty.filtered.title", { query: filters.q })
            : t("git.repos.empty.filtered.titleScope")}
        </EmptyTitle>
      </EmptyHeader>
      <EmptyContent>
        <Button variant="outline" className="max-md:h-11" onClick={clear}>
          {t("git.repos.empty.filtered.action")}
        </Button>
      </EmptyContent>
    </Empty>
  ) : (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base">
          {t(canAdd ? "git.repos.empty.manage.title" : "git.repos.empty.view.title")}
        </EmptyTitle>
        <EmptyDescription>
          {t(canAdd ? "git.repos.empty.manage.body" : "git.repos.empty.view.body")}
        </EmptyDescription>
      </EmptyHeader>
      {canAdd ? (
        <EmptyContent>
          <Button className="max-md:h-11" onClick={onAdd}>
            <Plus aria-hidden="true" />
            {t("git.repos.add")}
          </Button>
        </EmptyContent>
      ) : null}
    </Empty>
  );

  const caption = filtered
    ? t("git.repos.countOf", { count: rows.length, total })
    : t("git.repos.count", { count: rows.length });

  return (
    <ResourceTable<GitRepo>
      resource="git"
      i18nKey="git.repos"
      title={t("git.title")}
      tableLabel={t("git.repos.title")}
      errorBody={t("git.repos.errorBody")}
      retryLabel={t("git.repos.retry")}
      columns={columns}
      query={{
        data: loaded ? { items: rows, total: rows.length, truncated: false } : undefined,
        isPending,
        isError,
        isFetching,
        refetch,
      }}
      envName=""
      getRowKey={(r) => r.id}
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
            label: t("git.repos.search.label"),
            placeholder: t("git.repos.search.placeholder"),
            clear: t("git.repos.search.clear"),
          }}
        />
      }
      empty={empty}
    />
  );
}

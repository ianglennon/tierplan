// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useInfiniteQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { TriangleAlert } from "lucide-react";
import { Fragment, type ReactNode, useId, useMemo, useState } from "react";
import { Trans } from "react-i18next";

import { isTagErrorType, type TagErrorType, TagsRequestError, tagsInfiniteQuery } from "@/api/tags";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { SearchBox } from "@/features/containers/container-list";
import { useEnvName } from "@/features/resources/use-env-name";
import { PlatformTable } from "@/features/tags/platform-chips";
import { TagRow } from "@/features/tags/tag-row";
import { useTagDetails } from "@/features/tags/use-tag-details";
import { useVisibleRows } from "@/features/tags/use-visible-rows";
import { useAnyEnvironmentPermission, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// The rows the list shows as skeletons while the first page loads (UI-SPEC "Shared state").
const SKELETON_ROWS = 8;

export interface TagBrowserProps {
  // The repository as it was typed: ghcr.io/acme/app or nginx.
  repo: string;
  // full is the page and the image's Tags tab; picker is the pull form's list.
  variant: "full" | "picker";
  // Environment context: the tags are read through the environment's routes with its
  // credentials, and the rows say what runs there and what its nodes cannot run (D-22, D-23).
  // Absent is the Registries context.
  envId?: string;
  // Registries context only: an environment whose own credentials count as well as the
  // instance's, without the list becoming that environment's.
  credentialEnvId?: string;
  // Full: what Pull does with a reference (repo:tag). Without it the rows offer no Pull.
  onPull?: (ref: string) => void;
  // Picker: what choosing a tag does, and the tag chosen.
  onSelect?: (tag: string) => void;
  selected?: string;
  // The filter text and the Hide switch. A page that keeps them in its address passes both
  // and hears of a change in onFilterChange; without onFilterChange the browser keeps them.
  q?: string;
  hide?: boolean;
  onFilterChange?: (patch: TagFilterPatch) => void;
}

// A change to the filter: a member that is present is the new value, and an undefined q
// clears the text.
export interface TagFilterPatch {
  q?: string | undefined;
  hide?: boolean;
}

function scopeLabel(t: (key: string) => string, kind: string, name: string): string {
  return kind === "instance" ? t("tags.credential.scopeInstance") : name;
}

// TagBrowser lists a repository's tags through the server, with the credential it picks for
// the repository (D-19, D-24a). The credential is named above the list and never leaves the
// server. A failure replaces the table with its own message and way forward.
export function TagBrowser({
  repo,
  variant,
  envId,
  credentialEnvId,
  onPull,
  onSelect,
  selected,
  q: qProp,
  hide: hideProp,
  onFilterChange,
}: TagBrowserProps) {
  const t = useT();
  const [local, setLocal] = useState<{ q?: string; hide: boolean }>({ hide: true });
  const controlled = onFilterChange !== undefined;
  const q = controlled ? qProp : local.q;
  const hide = controlled ? hideProp !== false : local.hide;
  function change(patch: TagFilterPatch) {
    if (onFilterChange) onFilterChange(patch);
    else setLocal((prev) => ({ ...prev, ...patch }));
  }

  const query = useInfiniteQuery(tagsInfiniteQuery({ repo, envId, credentialEnvId, q, hide }));
  const picker = variant === "picker";
  // The Registries page, which has no environment, says which environments run each tag;
  // inside an environment the tag says what runs there.
  const showRunning = !picker && envId === undefined;
  const showHere = !picker && envId !== undefined;
  const extraColumns = showRunning || showHere;
  const envName = useEnvName(envId ?? "");

  const pages = query.data?.pages ?? [];
  const first = pages[0];
  const listed = useMemo(
    () => (query.data?.pages ?? []).flatMap((p) => p.tags ?? []),
    [query.data],
  );
  const { visible, observe } = useVisibleRows();
  const { ordered: tags, detailOf } = useTagDetails({
    repo,
    envId,
    credentialEnvId,
    tags: listed,
    visible,
  });
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(() => new Set());
  const panelPrefix = useId();
  function toggle(name: string) {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (!next.delete(name)) next.add(name);
      return next;
    });
  }

  const hidden = first?.hidden ?? 0;
  let body: ReactNode;
  if (query.isPending) {
    body = <SkeletonRows />;
  } else if (query.isError && !query.data) {
    body = <TagError error={query.error} repo={repo} onRetry={() => void query.refetch()} />;
  } else if (tags.length === 0) {
    body = (
      <EmptyList
        hide={hide}
        hidden={hidden}
        q={q}
        repo={first?.repo ?? repo}
        host={first?.host ?? ""}
        onShowHidden={() => change({ hide: false })}
        onClearFilter={() => change({ q: undefined })}
        onRetry={() => void query.refetch()}
      />
    );
  } else {
    body = (
      <div className={cn("overflow-x-auto", picker && "max-h-80 overflow-y-auto")}>
        <Table aria-label={t("tags.table.label", { repo: first?.repo ?? repo })}>
          <TableHeader>
            <TableRow>
              {picker ? null : <TableHead className="w-10" />}
              <TableHead>{t("tags.columns.tag")}</TableHead>
              {picker ? null : (
                <TableHead className="min-w-44">{t("tags.columns.digest")}</TableHead>
              )}
              <TableHead className="min-w-28">{t("tags.columns.created")}</TableHead>
              <TableHead className="min-w-48">{t("tags.columns.platforms")}</TableHead>
              {showRunning ? (
                <TableHead className="min-w-40">{t("tags.columns.runningIn")}</TableHead>
              ) : null}
              {showHere ? (
                <TableHead className="min-w-40">{t("tags.columns.runningHere")}</TableHead>
              ) : null}
              {picker ? null : (
                <TableHead className="w-12">
                  <span className="sr-only">{t("tags.columns.actions")}</span>
                </TableHead>
              )}
            </TableRow>
          </TableHeader>
          <TableBody>
            {tags.map((tag) => {
              const detail = detailOf(tag.name);
              const open = !picker && expanded.has(tag.name);
              const panelId = `${panelPrefix}-${tag.name}`;
              const platforms = detail?.platforms?.length
                ? detail.platforms
                : (tag.platforms ?? []);
              return (
                <Fragment key={tag.name}>
                  <TagRow
                    tag={tag}
                    detail={detail}
                    variant={variant}
                    selected={picker && selected === tag.name}
                    onSelect={onSelect}
                    showRunning={showRunning}
                    envId={envId}
                    envName={envName}
                    repo={repo}
                    onPull={onPull}
                    expanded={open}
                    panelId={panelId}
                    onToggle={toggle}
                    rowRef={observe}
                  />
                  {open && platforms.length > 1 ? (
                    <TableRow className="hover:bg-transparent">
                      <TableCell
                        id={panelId}
                        colSpan={extraColumns ? 7 : 6}
                        className="bg-muted/30 pl-12"
                      >
                        <PlatformTable
                          tag={tag.name}
                          digest={detail?.digest || tag.digest || ""}
                          platforms={platforms}
                        />
                      </TableCell>
                    </TableRow>
                  ) : null}
                </Fragment>
              );
            })}
          </TableBody>
        </Table>
      </div>
    );
  }

  return (
    <div className="flex min-w-0 flex-col gap-3">
      <CredentialLine
        pending={query.isPending}
        credential={first?.credential ?? null}
        shown={first !== undefined}
      />
      <div className="flex flex-wrap items-center gap-x-6 gap-y-3">
        <SearchBox
          value={q}
          onCommit={(next) => change({ q: next })}
          label={t("tags.filter.label")}
          placeholder={t("tags.filter.placeholder")}
          clearLabel={t("tags.filter.clear")}
        />
        <HideSwitch
          checked={hide}
          hidden={first ? hidden : 0}
          onChange={(next) => change({ hide: next })}
        />
      </div>
      <div className="rounded-lg border" aria-busy={query.isPending}>
        {body}
      </div>
      {first !== undefined && tags.length > 0 ? (
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          <p className="text-sm text-muted-foreground">
            {t("tags.caption", { count: tags.length })}
            {first.capped ? ` ${t("tags.capped", { count: first.limit ?? 10000 })}` : ""}
          </p>
          {query.hasNextPage ? (
            <Button
              variant="outline"
              size="sm"
              disabled={query.isFetchingNextPage}
              onClick={() => void query.fetchNextPage()}
            >
              {query.isFetchingNextPage ? <Spinner aria-hidden="true" /> : null}
              {query.isFetchingNextPage ? t("tags.paging.loading") : t("tags.paging.more")}
            </Button>
          ) : (
            <p className="text-sm text-muted-foreground">{t("tags.paging.end")}</p>
          )}
          {query.isFetchNextPageError ? (
            <p role="alert" className="text-sm text-destructive">
              {t("tags.paging.error")}
            </p>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

interface HideSwitchProps {
  checked: boolean;
  hidden: number;
  onChange: (next: boolean) => void;
}

// HideSwitch is the "Hide signature, attestation and cache tags" switch, on by default, with
// how many tags it is hiding beside it.
function HideSwitch({ checked, hidden, onChange }: HideSwitchProps) {
  const t = useT();
  const id = useId();
  return (
    <div className="flex items-center gap-2 max-md:min-h-11">
      <Switch id={id} checked={checked} onCheckedChange={onChange} />
      <Label htmlFor={id}>{t("tags.hide.label")}</Label>
      {checked && hidden > 0 ? (
        <span className="text-xs text-muted-foreground">{t("tags.hide.count", { n: hidden })}</span>
      ) : null}
    </div>
  );
}

interface EmptyListProps {
  hide: boolean;
  hidden: number;
  q: string | undefined;
  repo: string;
  host: string;
  onShowHidden: () => void;
  onClearFilter: () => void;
  onRetry: () => void;
}

// EmptyList says why there is nothing to list and offers the way out: only tags the switch
// hides are left, the filter matches nothing, or the repository has no tags.
function EmptyList({
  hide,
  hidden,
  q,
  repo,
  host,
  onShowHidden,
  onClearFilter,
  onRetry,
}: EmptyListProps) {
  const t = useT();
  let title: string;
  let description: string | undefined;
  let action: { label: string; run: () => void };
  if (hide && hidden > 0) {
    title = t("tags.empty.onlyHidden.title");
    action = { label: t("tags.empty.onlyHidden.show"), run: onShowHidden };
  } else if (q) {
    title = t("tags.empty.noMatch.title", { query: q });
    action = { label: t("tags.empty.noMatch.clear"), run: onClearFilter };
  } else {
    title = t("tags.empty.none.title");
    description = t("tags.empty.none.body", { repo, host });
    action = { label: t("tags.empty.none.retry"), run: onRetry };
  }
  return (
    <Empty>
      <EmptyHeader>
        <EmptyTitle className="break-all">{title}</EmptyTitle>
        {description ? <EmptyDescription>{description}</EmptyDescription> : null}
      </EmptyHeader>
      <EmptyContent>
        <Button variant="outline" onClick={action.run}>
          {action.label}
        </Button>
      </EmptyContent>
    </Empty>
  );
}

function SkeletonRows() {
  const t = useT();
  return (
    <Table aria-label={t("tags.columns.tag")}>
      <TableBody>
        {Array.from({ length: SKELETON_ROWS }, (_, i) => (
          // The rows are identical placeholders, so their position is their identity.
          // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows
          <TableRow key={i} className="h-9">
            <TableCell>
              <Skeleton className="h-4 w-40" />
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

interface CredentialLineProps {
  pending: boolean;
  credential: { id: string; name: string; scope: { kind: string; envName?: string | null } } | null;
  // Whether an answer has arrived: only then is the line either of its two sentences.
  shown: boolean;
}

// CredentialLine says which stored credential the registry was asked with, or that it was
// asked without one (UI-SPEC "Browse a repository"). The name opens the credential.
function CredentialLine({ pending, credential, shown }: CredentialLineProps) {
  const t = useT();
  if (pending) return <Skeleton className="h-4 w-64" />;
  if (!shown) return null;
  if (!credential) {
    return <p className="text-sm text-muted-foreground">{t("tags.credential.anonymous")}</p>;
  }
  const scope = scopeLabel(t, credential.scope.kind, credential.scope.envName ?? "");
  return (
    <p className="text-sm break-all">
      <Trans
        i18nKey="tags.credential.using"
        ns="tags"
        values={{ name: credential.name, scope }}
        components={{
          cred: (
            <Link
              to="/registries"
              search={{ credential: credential.id }}
              className="underline underline-offset-4"
            />
          ),
        }}
      />
    </p>
  );
}

interface TagErrorProps {
  error: unknown;
  repo: string;
  onRetry: () => void;
}

// TagError is the Alert that stands in place of the table: the mapped sentence for the
// failure and its way forward. What the registry said is never shown.
function TagError({ error, repo, onRetry }: TagErrorProps) {
  const t = useT();
  const canAddInstance = usePermission("registries.instance");
  const canAddEnvironment = useAnyEnvironmentPermission("registries.manage");
  const err = error instanceof TagsRequestError ? error : new TagsRequestError(0);
  const kind: TagErrorType | "ref_invalid" | "generic" = isTagErrorType(err.type)
    ? err.type
    : err.type === "ref_invalid"
      ? "ref_invalid"
      : "generic";
  const cred = err.credential;
  const values = {
    host: err.host,
    repo: err.repo || repo,
    name: cred?.name ?? "",
    scope: cred ? scopeLabel(t, cred.scopeKind, cred.scopeName) : "",
  };

  let action: ReactNode = null;
  if (kind === "login_refused" && cred) {
    action = (
      <Link
        to="/registries"
        search={{ credential: cred.id }}
        className="underline underline-offset-4"
      >
        {t("tags.errors.login_refused.action")}
      </Link>
    );
  } else if (kind === "needs_login") {
    action =
      canAddInstance || canAddEnvironment ? (
        <Link
          to="/registries"
          search={{ credential: "new" }}
          className="underline underline-offset-4"
        >
          {t("tags.errors.needs_login.action")}
        </Link>
      ) : null;
  } else if (
    kind === "registry_unreachable" ||
    kind === "registry_tls" ||
    kind === "rate_limited"
  ) {
    action = (
      <Button variant="outline" size="sm" onClick={onRetry}>
        {t(`tags.errors.${kind}.action`)}
      </Button>
    );
  } else if (kind === "generic") {
    action = (
      <Button variant="outline" size="sm" onClick={onRetry}>
        {t("tags.errors.generic.action")}
      </Button>
    );
  }

  return (
    <Alert variant="destructive" className="border-0">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle className="break-all">{t(`tags.errors.${kind}.message`, values)}</AlertTitle>
      {action ? <AlertDescription>{action}</AlertDescription> : null}
    </Alert>
  );
}

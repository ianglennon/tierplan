// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useInfiniteQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { TriangleAlert } from "lucide-react";
import type { ReactNode } from "react";
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
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { TagRow } from "@/features/tags/tag-row";
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
  // An environment whose own credentials count as well as the instance's.
  envId?: string;
  // Picker: what choosing a tag does, and the tag chosen.
  onSelect?: (tag: string) => void;
  selected?: string;
}

function scopeLabel(t: (key: string) => string, kind: string, name: string): string {
  return kind === "instance" ? t("tags.credential.scopeInstance") : name;
}

// TagBrowser lists a repository's tags through the server, with the credential it picks for
// the repository (D-19, D-24a). The credential is named above the list and never leaves the
// server. A failure replaces the table with its own message and way forward.
export function TagBrowser({ repo, variant, envId, onSelect, selected }: TagBrowserProps) {
  const t = useT();
  const query = useInfiniteQuery(tagsInfiniteQuery({ repo, envId }));
  const picker = variant === "picker";

  const pages = query.data?.pages ?? [];
  const first = pages[0];
  const tags = pages.flatMap((p) => p.tags ?? []);

  let body: ReactNode;
  if (query.isPending) {
    body = <SkeletonRows />;
  } else if (query.isError) {
    body = <TagError error={query.error} repo={repo} onRetry={() => void query.refetch()} />;
  } else if (tags.length === 0) {
    body = (
      <Empty>
        <EmptyHeader>
          <EmptyTitle>{t("tags.empty.none.title")}</EmptyTitle>
          <EmptyDescription>
            {t("tags.empty.none.body", { repo: first?.repo ?? repo, host: first?.host ?? "" })}
          </EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button variant="outline" onClick={() => void query.refetch()}>
            {t("tags.empty.none.retry")}
          </Button>
        </EmptyContent>
      </Empty>
    );
  } else {
    body = (
      <div className={cn("overflow-x-auto", picker && "max-h-80 overflow-y-auto")}>
        <Table aria-label={t("tags.table.label", { repo: first?.repo ?? repo })}>
          <TableHeader>
            <TableRow>
              <TableHead>{t("tags.columns.tag")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {tags.map((tag) => (
              <TagRow
                key={tag.name}
                tag={tag}
                variant={variant}
                selected={picker && selected === tag.name}
                onSelect={onSelect}
              />
            ))}
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
      <div className="rounded-lg border" aria-busy={query.isPending}>
        {body}
      </div>
      {first !== undefined && tags.length > 0 ? (
        <p className="text-sm text-muted-foreground">
          {t("tags.caption", { count: tags.length })}
          {first.capped ? ` ${t("tags.capped", { count: 10000 })}` : ""}
        </p>
      ) : null}
    </div>
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

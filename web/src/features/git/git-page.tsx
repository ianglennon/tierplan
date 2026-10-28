// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";

import { environmentsQuery } from "@/api/environments";
import { type GitRepo, gitReposQuery, useTestGitRepo } from "@/api/git";
import { NoPermission } from "@/components/full-page-state";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { MAX_QUERY } from "@/features/containers/list-search";
import type { GitFiltersPatch, ScopeOption } from "@/features/git/git-table-parts";
import { useFailureReason } from "@/features/git/repo-test-result";
import { RepositorySheet, RepositorySheetPlaceholder } from "@/features/git/repository-sheet";
import { RepositoryTable } from "@/features/git/repository-table";
import {
  hasPermission,
  useAnyEnvironmentPermission,
  useDocumentTitle,
  useMe,
  usePermission,
} from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";
import { z } from "@/lib/zod-config";

// The longest value a search parameter holds that is an ID: a UUID is 36.
const MAX_ID = 64;

// The Repositories route's search (UI-SPEC "Screens"): the search text, the Scope filter
// ("instance" or an environment's ID) and the repository whose sheet is open ("new" for the add
// form, an ID for a repository). A value that does not fit is dropped, so a stale or hand-edited
// link shows the default list instead of an error.
export const repositoriesSearchSchema = z.object({
  q: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
  scope: z.string().min(1).max(MAX_ID).optional().catch(undefined),
  repo: z.string().min(1).max(MAX_ID).optional().catch(undefined),
});

export type RepositoriesSearch = z.output<typeof repositoriesSearchSchema>;

// A change to the search: a key set to undefined leaves the URL.
export type RepositoriesSearchPatch = {
  [K in keyof RepositoriesSearch]?: RepositoriesSearch[K] | undefined;
};

// GitTabs are the Repositories and Credentials tabs: Link triggers, so each is a URL of its own.
export function GitTabs() {
  const t = useT();
  return (
    <Tabs value="" className="gap-4">
      <TabsList variant="line" aria-label={t("git.tabs.label")} className="h-9 max-md:h-11">
        <TabsTrigger
          value="repositories"
          asChild
          className="data-active:text-accent-ink after:bg-accent-ink max-md:h-11"
        >
          <Link
            to="/git"
            activeOptions={{ exact: true }}
            activeProps={{ "data-active": true }}
            inactiveProps={{ "data-active": undefined }}
          >
            {t("git.tabs.repositories")}
          </Link>
        </TabsTrigger>
      </TabsList>
    </Tabs>
  );
}

// The page's caption and tabs, drawn under the title.
function PageBanner() {
  const t = useT();
  return (
    <>
      <p className="max-w-prose text-sm text-muted-foreground">{t("git.caption")}</p>
      <GitTabs />
    </>
  );
}

interface GitPageProps {
  search: RepositoriesSearch;
  onSearchChange: (patch: RepositoriesSearchPatch) => void;
}

// GitPage is the Repositories tab of the Git page, which everyone who can read git connections can
// open. The server limits the rows to what the caller may see; this only decides what to show.
// Adding is for an instance administrator (git.instance) and an environment Admin (git.manage),
// and everyone else sees the same table read-only.
export function GitPage({ search, onSearchChange }: GitPageProps) {
  const t = useT();
  useDocumentTitle(t("git.title"));
  const allowed = useAnyEnvironmentPermission("git.read");
  const canAddInstance = usePermission("git.instance");
  const canAddEnvironment = useAnyEnvironmentPermission("git.manage");
  const canAdd = canAddInstance || canAddEnvironment;
  const repos = useQuery({ ...gitReposQuery, enabled: allowed });
  const environments = useQuery({ ...environmentsQuery, enabled: allowed });
  const tester = useTestGitRepo();
  const reason = useFailureReason();
  const [testingId, setTestingId] = useState<string | null>(null);
  // The repository whose delete confirmation is open.
  const [, setDeleting] = useState<GitRepo | null>(null);

  const me = useMe();
  // The Scope filter lists the environments the user can read git connections on. The server
  // already limits the list; this keeps the filter from offering a scope that could only be empty.
  const scopes = useMemo<ScopeOption[]>(
    () =>
      (environments.data?.items ?? [])
        .filter((e) => hasPermission(me.data, "git.read", e.id))
        .map((e) => ({ id: e.id, name: e.name }))
        .sort((a, b) => a.name.localeCompare(b.name)),
    [environments.data, me.data],
  );

  // ?repo=new is the add form, which only an instance administrator or an environment Admin has.
  // For anyone else the parameter is dropped, so a shared link shows the list.
  const addDenied = allowed && !canAdd && search.repo === "new";
  useEffect(() => {
    if (addDenied) onSearchChange({ repo: undefined });
  }, [addDenied, onSearchChange]);

  if (!allowed) return <NoPermission />;

  const closeSheet = () => onSearchChange({ repo: undefined });
  const listed = repos.data?.find((r) => r.id === search.repo);

  // Test is the row action: it runs the test of the saved repository and shows the result as a
  // toast. A failed test never changes anything else.
  function test(repo: GitRepo) {
    if (tester.isPending) return;
    setTestingId(repo.id);
    tester.mutate(
      { id: repo.id, envId: repo.scope.envId ?? undefined },
      {
        onSuccess: (result) => {
          if (result.failure) {
            toastError(t("git.repos.testToast.failed", { reason: reason(result.failure) }));
          } else {
            toastSuccess(
              t("git.repos.testToast.passed", {
                branches: result.branchCount,
                tags: result.tagCount,
              }),
            );
          }
        },
        onError: (err) =>
          toastError(
            t(
              err.status === 429
                ? "git.test.errors.rateLimited"
                : err.status === 403
                  ? "git.test.errors.permission"
                  : "git.test.errors.generic",
            ),
          ),
        onSettled: () => setTestingId(null),
      },
    );
  }

  let sheet: React.ReactNode = null;
  if (search.repo === "new") {
    sheet = canAdd ? <RepositorySheet mode="add" onClose={closeSheet} /> : null;
  } else if (search.repo) {
    if (listed) {
      sheet = (
        <RepositorySheet
          mode={listed.readOnly ? "view" : "edit"}
          repo={listed}
          onClose={closeSheet}
        />
      );
    } else if (repos.isPending || repos.isError) {
      sheet = (
        <RepositorySheetPlaceholder
          state={repos.isPending ? "loading" : "error"}
          onClose={closeSheet}
          onRetry={() => void repos.refetch()}
          retrying={repos.isFetching}
        />
      );
    } else {
      sheet = (
        <RepositorySheetPlaceholder
          state="gone"
          onClose={closeSheet}
          onRetry={() => undefined}
          retrying={false}
        />
      );
    }
  }

  return (
    <>
      <RepositoryTable
        repos={repos.data}
        isPending={repos.isPending}
        isError={repos.isError}
        isFetching={repos.isFetching}
        refetch={repos.refetch}
        filters={{ q: search.q, scope: search.scope }}
        onFiltersChange={onSearchChange as (patch: GitFiltersPatch) => void}
        scopes={scopes}
        canAdd={canAdd}
        banner={<PageBanner />}
        onAdd={() => onSearchChange({ repo: "new" })}
        onOpen={(id) => onSearchChange({ repo: id })}
        onTest={test}
        onDelete={setDeleting}
        testingId={testingId}
      />
      {sheet}
    </>
  );
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";

import { environmentsQuery } from "@/api/environments";
import {
  type GitCredential,
  type GitRepo,
  gitCredentialsQuery,
  gitReposQuery,
  useTestGitRepo,
} from "@/api/git";
import { NoPermission } from "@/components/full-page-state";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { MAX_QUERY } from "@/features/containers/list-search";
import { CredentialSheet, PlaceholderCredentialSheet } from "@/features/git/credential-sheet";
import { CredentialTable } from "@/features/git/credential-table";
import { DeleteCredentialDialog } from "@/features/git/delete-credential-dialog";
import { DeleteRepositoryDialog } from "@/features/git/delete-repository-dialog";
import type { GitFiltersPatch, ScopeOption } from "@/features/git/git-table-parts";
import { useFailureReason } from "@/features/git/repo-test-result";
import { PlaceholderRepositorySheet, RepositorySheet } from "@/features/git/repository-sheet";
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

// The Credentials route's search: the same, with the credential whose sheet is open.
export const credentialsSearchSchema = z.object({
  q: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
  scope: z.string().min(1).max(MAX_ID).optional().catch(undefined),
  credential: z.string().min(1).max(MAX_ID).optional().catch(undefined),
});

export type CredentialsSearch = z.output<typeof credentialsSearchSchema>;

// What the page reads from either tab's search.
export type GitSearch = RepositoriesSearch & CredentialsSearch;

// A change to the search: a key set to undefined leaves the URL.
export type RepositoriesSearchPatch = {
  [K in keyof GitSearch]?: GitSearch[K] | undefined;
};
export type GitSearchPatch = RepositoriesSearchPatch;

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
        <TabsTrigger
          value="credentials"
          asChild
          className="data-active:text-accent-ink after:bg-accent-ink max-md:h-11"
        >
          <Link
            to="/git/credentials"
            activeOptions={{ exact: true }}
            activeProps={{ "data-active": true }}
            inactiveProps={{ "data-active": undefined }}
          >
            {t("git.tabs.credentials")}
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
  // The tab the route shows; Repositories unless the route is /git/credentials.
  tab?: "repositories" | "credentials";
  search: GitSearch;
  onSearchChange: (patch: GitSearchPatch) => void;
}

// GitPage is the Repositories tab of the Git page, which everyone who can read git connections can
// open. The server limits the rows to what the caller may see; this only decides what to show.
// Adding is for an instance administrator (git.instance) and an environment Admin (git.manage),
// and everyone else sees the same table read-only.
export function GitPage({ tab = "repositories", search, onSearchChange }: GitPageProps) {
  const t = useT();
  useDocumentTitle(t("git.title"));
  const allowed = useAnyEnvironmentPermission("git.read");
  const canAddInstance = usePermission("git.instance");
  const canAddEnvironment = useAnyEnvironmentPermission("git.manage");
  const canAdd = canAddInstance || canAddEnvironment;
  const navigate = useNavigate();
  const repos = useQuery({ ...gitReposQuery, enabled: allowed });
  const credentials = useQuery({ ...gitCredentialsQuery, enabled: allowed });
  const environments = useQuery({ ...environmentsQuery, enabled: allowed });
  const tester = useTestGitRepo();
  const reason = useFailureReason();
  const [testingId, setTestingId] = useState<string | null>(null);
  // The repository or credential whose delete confirmation is open.
  const [deletingRepo, setDeletingRepo] = useState<GitRepo | null>(null);
  const [deletingCredential, setDeletingCredential] = useState<GitCredential | null>(null);

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
  const sheetKey = tab === "repositories" ? "repo" : "credential";
  const open = search[sheetKey];
  const addDenied = allowed && !canAdd && open === "new";
  useEffect(() => {
    if (addDenied) onSearchChange({ [sheetKey]: undefined });
  }, [addDenied, onSearchChange, sheetKey]);

  // The repositories that use each credential, for the credentials' Used by popover.
  const usedByNames = useMemo(() => {
    const out = new Map<string, string[]>();
    for (const r of repos.data ?? []) {
      if (!r.credential) continue;
      out.set(r.credential.id, [...(out.get(r.credential.id) ?? []), r.name]);
    }
    for (const names of out.values()) names.sort((a, b) => a.localeCompare(b));
    return out;
  }, [repos.data]);

  if (!allowed) return <NoPermission />;

  const closeSheet = () => onSearchChange({ [sheetKey]: undefined });
  const listed = repos.data?.find((r) => r.id === search.repo);
  const listedCredential = credentials.data?.find((c) => c.id === search.credential);

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
  if (tab === "repositories") {
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
      } else {
        sheet = (
          <PlaceholderRepositorySheet
            state={repos.isPending ? "loading" : repos.isError ? "error" : "gone"}
            onClose={closeSheet}
            onRetry={() => void repos.refetch()}
            retrying={repos.isFetching}
          />
        );
      }
    }
  } else if (search.credential === "new") {
    sheet = canAdd ? <CredentialSheet mode="add" onClose={closeSheet} /> : null;
  } else if (search.credential) {
    if (listedCredential) {
      sheet = (
        <CredentialSheet
          mode={listedCredential.readOnly ? "view" : "edit"}
          credential={listedCredential}
          onClose={closeSheet}
        />
      );
    } else {
      sheet = (
        <PlaceholderCredentialSheet
          state={credentials.isPending ? "loading" : credentials.isError ? "error" : "gone"}
          onClose={closeSheet}
          onRetry={() => void credentials.refetch()}
          retrying={credentials.isFetching}
        />
      );
    }
  }

  return (
    <>
      {tab === "repositories" ? (
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
          onOpenCredential={(id) =>
            void navigate({ to: "/git/credentials", search: { credential: id } })
          }
          onTest={test}
          onDelete={setDeletingRepo}
          testingId={testingId}
        />
      ) : (
        <CredentialTable
          credentials={credentials.data}
          isPending={credentials.isPending}
          isError={credentials.isError}
          isFetching={credentials.isFetching}
          refetch={credentials.refetch}
          filters={{ q: search.q, scope: search.scope }}
          onFiltersChange={onSearchChange as (patch: GitFiltersPatch) => void}
          scopes={scopes}
          canAdd={canAdd}
          banner={<PageBanner />}
          onAdd={() => onSearchChange({ credential: "new" })}
          onOpen={(id) => onSearchChange({ credential: id })}
          onDelete={setDeletingCredential}
          usedByNames={usedByNames}
        />
      )}
      <DeleteRepositoryDialog
        repository={deletingRepo}
        open={deletingRepo !== null}
        onClose={() => setDeletingRepo(null)}
        onDone={() => {
          // Deleting the repository whose sheet is open leaves nothing to show.
          if (deletingRepo && deletingRepo.id === search.repo) closeSheet();
        }}
      />
      <DeleteCredentialDialog
        credential={deletingCredential}
        open={deletingCredential !== null}
        onClose={() => setDeletingCredential(null)}
        onDone={() => {
          if (deletingCredential && deletingCredential.id === search.credential) closeSheet();
        }}
      />
      {sheet}
    </>
  );
}

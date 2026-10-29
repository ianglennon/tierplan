// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { CircleX } from "lucide-react";
import { useMemo, useState } from "react";

import { environmentsQuery } from "@/api/environments";
import {
  type GitRefKind,
  type GitRequestError,
  gitEnvReposQuery,
  gitTreeQuery,
  useCreateGitStack,
} from "@/api/git";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { SettingsCard } from "@/features/environments/settings-card";
import { RepositorySheet } from "@/features/git/repository-sheet";
import { CheckSummary, useGitSourceCheck } from "@/features/git-source/check-summary";
import { ComposeFileList } from "@/features/git-source/compose-file-list";
import { FolderBrowser } from "@/features/git-source/folder-browser";
import { RepoCard } from "@/features/git-source/repo-card";
import {
  RequiredVariablesLine,
  type TableVar,
  VariablesPreview,
} from "@/features/git-source/variables-preview";
import { useEnvName } from "@/features/resources/use-env-name";
import { EditorFooter } from "@/features/service-editor/editor-footer";
import { hasPermission, useDocumentTitle, useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// A commit is named by 7 to 40 hex characters; a branch or tag by anything the list holds.
const COMMIT = /^[0-9a-f]{7,40}$/i;
// The full commit ID the top of the page resolved a ref to.
const FULL_COMMIT = /^[0-9a-f]{40}$/;

interface GitSourcePageProps {
  envId: string;
  // The new stack's name, from the create dialog.
  name: string;
  mode: "create";
}

function Breadcrumb({ envId }: { envId: string }) {
  const t = useT();
  const link =
    "rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none";
  return (
    <nav aria-label={t("stacks.detail.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link to="/env/$envId/stacks" params={{ envId }} className={link}>
            {t("gitSource.page.breadcrumbRoot")}
          </Link>
        </li>
        <li>
          <span aria-current="page">{t("gitSource.page.breadcrumbCurrent")}</span>
        </li>
      </ol>
    </nav>
  );
}

// GitSourcePage is the page a stack is made from a folder of a git repository on (UI-SPEC "From
// git page"): the repository and the ref it follows, the folder and its compose files in merge
// order, then Create stack. The choices live in this component's state and nowhere else; the URL
// holds the name alone. Nothing is created until Create stack, and nothing is deployed after it.
export function GitSourcePage(props: GitSourcePageProps) {
  // Another stack or environment is another page: the choices are never carried over.
  return <GitSourcePageBody key={`${props.envId}/${props.name}`} {...props} />;
}

function GitSourcePageBody({ envId, name }: GitSourcePageProps) {
  const t = useT();
  const navigate = useNavigate();
  const envName = useEnvName(envId);
  const { data: me } = useMe();
  const { data: environments } = useQuery(environmentsQuery);
  const repos = useQuery(gitEnvReposQuery(envId));
  const create = useCreateGitStack(envId);
  useDocumentTitle(t("gitSource.page.documentTitle"));

  const [repoId, setRepoId] = useState("");
  const [kind, setKind] = useState<GitRefKind>("branch");
  const [gitRef, setGitRef] = useState("");
  // The folder chosen as the stack's root: null while none is, "" for the repository root.
  const [folder, setFolder] = useState<string | null>(null);
  const [files, setFiles] = useState<string[]>([]);
  const [adding, setAdding] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);
  // The values typed for the stack's variables. They live in this component's state alone: they go
  // to the check and to the create, are never shown back, and go with the page.
  const [table, setTable] = useState<TableVar[]>([]);

  const list = repos.data ?? [];
  const repo = list.find((r) => r.id === repoId);
  const canAdd = hasPermission(me, "git.manage", envId);
  const envType = environments?.items?.find((e) => e.id === envId)?.envType;
  const target = envType
    ? t(envType === "standalone" ? "stacks.target.standalone" : "stacks.target.swarm")
    : "";

  const refChosen = kind === "commit" ? COMMIT.test(gitRef) : gitRef !== "";
  const ready = repo !== undefined && refChosen && folder !== null && files.length > 0;

  // The commit the ref resolved to at the top of the page. The check reads that commit, so the
  // summary, the variables and the create all speak of the same files.
  const resolved = useQuery({
    ...gitTreeQuery(envId, repo?.id ?? "", kind, gitRef, ""),
    enabled: repo !== undefined && refChosen,
  });
  const commit = resolved.data?.commit;
  // A variable with no value is not sent: a blank would hide the repository's .env value.
  const sentVars = useMemo(() => table.filter((v) => v.value !== ""), [table]);
  const request = useMemo(
    () =>
      ready && repo && folder !== null
        ? {
            name,
            repoId: repo.id,
            refKind: kind,
            ref: gitRef,
            folder,
            composeFiles: files,
            ...(commit && FULL_COMMIT.test(commit) ? { commit } : {}),
            ...(sentVars.length > 0 ? { vars: sentVars } : {}),
          }
        : null,
    [ready, repo, name, kind, gitRef, folder, files, commit, sentVars],
  );
  const check = useGitSourceCheck(envId, request);

  function clearChoices() {
    setFolder(null);
    setFiles([]);
  }
  function chooseRepo(id: string) {
    setRepoId(id);
    setGitRef("");
    clearChoices();
  }
  function changeKind(next: GitRefKind) {
    setKind(next);
    setGitRef("");
    clearChoices();
  }
  function chooseFolder(path: string) {
    if (path !== folder) {
      setFolder(path);
      setFiles([]);
    }
  }

  function refusalText(err: GitRequestError): string {
    switch (err.type) {
      case "stack_name_taken":
        return t("stacks.create.errors.nameTaken", { name, env: envName });
      case "stack_name_clash":
        return t("stackLists.create.errors.nameClash", { name });
      case "stack_name_invalid":
        return t("gitSource.errors.nameInvalid");
    }
    if (err.status === 0) return t("stacks.create.errors.lost");
    if (err.status === 403) return t("stacks.create.errors.permission");
    if (err.status === 422 || err.status === 404) return t("gitSource.errors.createFailed");
    return t("stacks.create.errors.generic");
  }

  function submit() {
    if (!ready || create.isPending || folder === null) return;
    setRefusal(null);
    create.mutate(
      {
        name,
        repoId,
        refKind: kind,
        ref: gitRef,
        folder,
        composeFiles: files,
        ...(sentVars.length > 0 ? { vars: sentVars } : {}),
      },
      {
        onSuccess: (made) => {
          setTable([]);
          toastSuccess(t("stacks.create.success", { name }));
          void navigate({
            to: "/env/$envId/stacks/$stackId",
            params: { envId, stackId: made.stackId },
            replace: true,
          });
        },
        onError: (err) => setRefusal(refusalText(err)),
      },
    );
  }

  const createButton = (
    <Button
      type="button"
      className="max-md:h-11"
      aria-disabled={!ready || undefined}
      aria-busy={create.isPending || undefined}
      onClick={submit}
    >
      {create.isPending ? <Spinner aria-hidden="true" /> : null}
      {create.isPending ? t("gitSource.footer.pending") : t("gitSource.footer.create")}
    </Button>
  );

  return (
    <div className="flex flex-col gap-6">
      <div className="flex w-full max-w-6xl flex-col gap-6">
        <header className="flex flex-col gap-2">
          <Breadcrumb envId={envId} />
          <h2 className="line-clamp-2 min-w-0 break-all text-2xl font-semibold">
            {t("gitSource.page.title", { name })}
          </h2>
          <p className="max-w-prose text-sm text-muted-foreground">
            {t("gitSource.page.body", { name })}
          </p>
          {target ? (
            <p className="text-sm text-muted-foreground">
              {t("stacks.create.target", { name: envName, target })}
            </p>
          ) : null}
        </header>

        {refusal ? (
          <Alert variant="destructive" role="alert">
            <CircleX aria-hidden="true" />
            <AlertDescription className="break-words">{refusal}</AlertDescription>
          </Alert>
        ) : null}

        <SettingsCard title={t("gitSource.card1.title")}>
          <p className="text-sm text-muted-foreground">{t("gitSource.card1.body")}</p>
          <RepoCard
            envId={envId}
            repos={list}
            repoId={repoId}
            onRepoChange={chooseRepo}
            onAdd={canAdd ? () => setAdding(true) : undefined}
            kind={kind}
            gitRef={gitRef}
            onKindChange={changeKind}
            onRefChange={setGitRef}
            clears={folder !== null}
            envName={envName}
            loaded={repos.isSuccess}
            loadError={
              repos.isError && !repos.data
                ? { retrying: repos.isFetching, onRetry: () => void repos.refetch() }
                : undefined
            }
          />
        </SettingsCard>

        <SettingsCard title={t("gitSource.card2.title")}>
          <p className="text-sm text-muted-foreground">{t("gitSource.card2.body")}</p>
          {repo && refChosen ? (
            <>
              <FolderBrowser
                envId={envId}
                repoId={repo.id}
                repoName={repo.name}
                kind={kind}
                gitRef={gitRef}
                folder={folder}
                onUse={chooseFolder}
              />
              {folder !== null ? (
                <ComposeFileList
                  key={`${repo.id}/${kind}/${folder}`}
                  envId={envId}
                  repoId={repo.id}
                  kind={kind}
                  gitRef={gitRef}
                  folder={folder}
                  files={files}
                  onChange={setFiles}
                />
              ) : null}
              {request ? <CheckSummary state={check} repoName={repo.name} /> : null}
            </>
          ) : (
            <p className="text-sm text-muted-foreground">
              {t(repo ? "gitSource.card2.firstRef" : "gitSource.card2.firstRepository")}
            </p>
          )}
        </SettingsCard>

        <VariablesPreview
          vars={check.check?.vars ?? null}
          value={table}
          onChange={setTable}
          enabled={files.length > 0}
          checking={check.checking}
        />

        <RequiredVariablesLine vars={check.check?.vars ?? null} />
      </div>

      <EditorFooter>
        <p className="me-auto min-w-0 text-sm break-words text-muted-foreground max-md:hidden">
          {t("gitSource.footer.note")}
        </p>
        <Button asChild variant="outline" className="max-md:h-11">
          <Link to="/env/$envId/stacks" params={{ envId }}>
            {t("gitSource.footer.cancel")}
          </Link>
        </Button>
        <NeedsTooltip reason={ready ? undefined : t("gitSource.footer.needsAll")}>
          {createButton}
        </NeedsTooltip>
      </EditorFooter>

      {adding ? (
        <RepositorySheet
          mode="add"
          defaultScope={envId}
          onSaved={(saved) => chooseRepo(saved.id)}
          onClose={() => setAdding(false)}
        />
      ) : null}
    </div>
  );
}

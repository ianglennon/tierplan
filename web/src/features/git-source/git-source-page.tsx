// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useBlocker, useNavigate } from "@tanstack/react-router";
import { CircleX } from "lucide-react";
import { useMemo, useRef, useState } from "react";

import { environmentsQuery } from "@/api/environments";
import {
  type GitRefKind,
  type GitRequestError,
  gitEnvReposQuery,
  gitTreeQuery,
  useCreateGitStack,
} from "@/api/git";
import { useConnectStackGit } from "@/api/stack-git";
import { draftKey, draftQuery, type StackDraft, stackQuery } from "@/api/stacks";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { SettingsCard } from "@/features/environments/settings-card";
import { RepositorySheet } from "@/features/git/repository-sheet";
import { AddOverlayDialog } from "@/features/git-source/add-overlay-dialog";
import { CheckSummary, useGitSourceCheck } from "@/features/git-source/check-summary";
import { ComposeFileList } from "@/features/git-source/compose-file-list";
import { FolderBrowser } from "@/features/git-source/folder-browser";
import {
  choiceOf,
  type LeftoverChoice,
  type LeftoverChoices,
  LeftoverFilesCard,
} from "@/features/git-source/leftover-files-card";
import { OverlayFilesCard } from "@/features/git-source/overlay-files-card";
import { RepoCard } from "@/features/git-source/repo-card";
import {
  RequiredVariablesLine,
  type TableVar,
  VariablesPreview,
} from "@/features/git-source/variables-preview";
import { useEnvName } from "@/features/resources/use-env-name";
import { EditorFooter } from "@/features/service-editor/editor-footer";
import { hasPermission, useDocumentTitle, useMe } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// A commit is named by 7 to 40 hex characters; a branch or tag by anything the list holds.
const COMMIT = /^[0-9a-f]{7,40}$/i;
// The full commit ID the top of the page resolved a ref to.
const FULL_COMMIT = /^[0-9a-f]{40}$/;

type GitSourcePageProps =
  | {
      envId: string;
      // The new stack's name, from the create dialog.
      name: string;
      mode: "create";
    }
  | {
      envId: string;
      // The inline or adopted stack that is to use a repository (D-24).
      stackId: string;
      mode: "convert";
    };

// What the convert page knows of the stack it is for: its name, and its draft as the page read it, whose version is what
// "Review and sync" sends.
interface ConvertTarget {
  stackId: string;
  draft: StackDraft;
}

function Breadcrumb({ envId, convert, name }: { envId: string; convert?: string; name: string }) {
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
        {convert ? (
          <li>
            <Link
              to="/env/$envId/stacks/$stackId"
              params={{ envId, stackId: convert }}
              className={link}
            >
              {name}
            </Link>
          </li>
        ) : null}
        <li>
          <span aria-current="page">
            {t(
              convert ? "gitSource.convert.breadcrumbCurrent" : "gitSource.page.breadcrumbCurrent",
            )}
          </span>
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
  if (props.mode === "convert") {
    return <ConvertGitSourcePage key={`${props.envId}/${props.stackId}`} {...props} />;
  }
  return (
    <GitSourcePageBody key={`${props.envId}/${props.name}`} envId={props.envId} name={props.name} />
  );
}

// The same page for a stack that exists (UI-SPEC "Convert page"): it waits for the stack and for its draft, whose files
// and variables it shows and whose version it sends, and then is the From git page with the existing-files card.
function ConvertGitSourcePage({ envId, stackId }: { envId: string; stackId: string }) {
  const t = useT();
  const stack = useQuery(stackQuery(envId, stackId));
  const draft = useQuery(draftQuery(envId, stackId));
  useDocumentTitle(t("gitSource.convert.documentTitle"));
  if (stack.data && draft.data) {
    if (stack.data.source === "git") {
      return (
        <div className="flex w-full max-w-6xl flex-col gap-4">
          <Alert variant="destructive" role="alert">
            <CircleX aria-hidden="true" />
            <AlertDescription className="flex flex-wrap items-center gap-3">
              <span className="break-words">
                {t("gitSource.convert.errors.alreadyGit", { name: stack.data.name })}
              </span>
              <Button asChild variant="outline" size="sm" className="max-md:h-11">
                <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId }}>
                  {t("gitSource.convert.errors.back")}
                </Link>
              </Button>
            </AlertDescription>
          </Alert>
        </div>
      );
    }
    return (
      <GitSourcePageBody
        key={draft.data.version}
        envId={envId}
        name={stack.data.name}
        convert={{ stackId, draft: draft.data }}
      />
    );
  }
  if (stack.isError || draft.isError) {
    return (
      <div className="flex w-full max-w-6xl flex-col gap-4">
        <Alert variant="destructive" role="alert">
          <CircleX aria-hidden="true" />
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span className="break-words">{t("gitSource.convert.errors.loadFailed")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={stack.isFetching || draft.isFetching}
              onClick={() => {
                void stack.refetch();
                void draft.refetch();
              }}
            >
              {t("gitSource.convert.errors.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      </div>
    );
  }
  return (
    <div className="flex w-full max-w-6xl flex-col gap-4" aria-busy="true">
      <Skeleton className="h-4 w-48" />
      <Skeleton className="h-8 w-96 max-w-full" />
      <Skeleton className="h-40 w-full" />
    </div>
  );
}

// The rows of the stack's variables as the table of the page starts: the values it holds, a secret one as <hidden>, which the
// server takes to mean the value it already keeps.
function tableOf(draft: StackDraft): TableVar[] {
  return (draft.vars ?? []).map((v) => ({ name: v.name, value: v.value, secret: v.secret }));
}

// The files the existing-files card lists: every file of the stack as it is today but its compose file, which the
// repository replaces. A file the repository supplied for a conversion that was started and left is not the stack's own.
function leftoverOf(draft: StackDraft): { path: string; secret: boolean }[] {
  const compose = new Set(draft.composeFiles ?? ["compose.yaml"]);
  return (draft.files ?? [])
    .filter((f) => !compose.has(f.path) && f.origin !== "repo")
    .map((f) => ({ path: f.path, secret: f.secret }));
}

function GitSourcePageBody({
  envId,
  name,
  convert,
}: {
  envId: string;
  name: string;
  convert?: ConvertTarget;
}) {
  const t = useT();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const envName = useEnvName(envId);
  const { data: me } = useMe();
  const { data: environments } = useQuery(environmentsQuery);
  const repos = useQuery(gitEnvReposQuery(envId));
  const create = useCreateGitStack(envId);
  const connect = useConnectStackGit(envId, convert?.stackId ?? "");
  // Pointing a stack at a repository is for an Admin of the environment (D-24); anyone who may read the stack sees the page.
  const adminNeeds = useNeeds("git.manage", envId);
  useDocumentTitle(t(convert ? "gitSource.convert.documentTitle" : "gitSource.page.documentTitle"));

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
  const [table, setTable] = useState<TableVar[]>(() => (convert ? tableOf(convert.draft) : []));
  // The overlay files added for paths the repository lacks, contents included: in memory only,
  // sent once with the create, never with a check, and gone with the page. The dialog is open
  // while dialogPath is set; "" is a blank one.
  const [overlays, setOverlays] = useState<{ path: string; content: string }[]>([]);
  const [dialogPath, setDialogPath] = useState<string | null>(null);
  // The conversion: what the person chose for each file of the stack as it is today (a file with no entry takes its
  // default), and the refusal the server gave, kept whole so that a conflict can name who changed the draft.
  const [choices, setChoices] = useState<LeftoverChoices>({});
  const [connectError, setConnectError] = useState<GitRequestError | null>(null);
  const leftover = useMemo(() => (convert ? leftoverOf(convert.draft) : []), [convert]);

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
  // What the stack folder holds, for refusing an overlay file that would shadow one of its files.
  const folderTree = useQuery({
    ...gitTreeQuery(envId, repo?.id ?? "", kind, gitRef, folder ?? ""),
    enabled: repo !== undefined && refChosen && folder !== null,
  });
  const repoPaths = useMemo(
    () => (folderTree.data?.entries ?? []).filter((e) => e.kind !== "dir").map((e) => e.name),
    [folderTree.data],
  );
  const repoPathSet = useMemo(() => new Set(repoPaths), [repoPaths]);
  // The files of the stack to keep as overlay files (a file the repository holds can't be kept).
  const keptPaths = useMemo(
    () => leftover.filter((f) => choiceOf(f, choices, repoPathSet) === "keep").map((f) => f.path),
    [leftover, choices, repoPathSet],
  );
  const addedPaths = useMemo(() => overlays.map((o) => o.path), [overlays]);
  // The check is told the paths of every overlay file, kept or added, never their contents.
  const overlayPaths = useMemo(() => [...keptPaths, ...addedPaths], [keptPaths, addedPaths]);
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
            ...(overlayPaths.length > 0 ? { overlayPaths } : {}),
          }
        : null,
    [ready, repo, name, kind, gitRef, folder, files, commit, sentVars, overlayPaths],
  );
  const check = useGitSourceCheck(envId, request);

  // Leaving with anything chosen or typed asks first (UI-SPEC "Unsaved-changes guard"): the
  // variable values and the overlay contents exist nowhere but in this page. The page's own
  // navigation after a create or a connect is not asked about, and nothing is asked while it runs.
  const startTable = useMemo(() => (convert ? tableOf(convert.draft) : []), [convert]);
  const dirty =
    repoId !== "" ||
    overlays.length > 0 ||
    Object.keys(choices).length > 0 ||
    JSON.stringify(table) !== JSON.stringify(startTable);
  const dirtyRef = useRef(false);
  dirtyRef.current = dirty;
  const finished = useRef(false);
  const blocker = useBlocker({
    shouldBlockFn: ({ current, next }) =>
      dirtyRef.current && !finished.current && current.pathname !== next.pathname,
    enableBeforeUnload: () => dirtyRef.current && !finished.current,
    withResolver: true,
  });

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

  function connectRefusal(err: GitRequestError): string {
    // A conflict is worded where it is shown, with its Reload button.
    if (err.type === "already_git") return t("gitSource.convert.errors.alreadyGit", { name });
    if (err.type === "overlay_clash") {
      const path = err.fields.find((f) => f.location === "files")?.message ?? "";
      return t("gitSource.convert.files.clash", { path });
    }
    if (err.status === 0) return t("stacks.create.errors.lost");
    if (err.status === 403) return t("gitSource.convert.errors.permission", { env: envName });
    if (err.status === 422 || err.status === 404) return t("gitSource.convert.errors.failed");
    return t("gitSource.convert.errors.generic");
  }

  // The draft moved while the person was choosing: read what is there now. The page starts again from it (it is keyed by
  // the draft's version), with the choices it offers by default.
  function reload() {
    if (!convert) return;
    setConnectError(null);
    void queryClient.refetchQueries({ queryKey: draftKey(envId, convert.stackId) });
  }

  function refusalText(err: GitRequestError): string {
    switch (err.type) {
      case "stack_name_taken":
        return t("stacks.create.errors.nameTaken", { name, env: envName });
      case "stack_name_clash":
        return t("stackLists.create.errors.nameClash", { name });
      case "stack_name_invalid":
        return t("gitSource.errors.nameInvalid");
      case "commit_moved":
        return t("gitSource.errors.commitMoved");
    }
    if (err.status === 0) return t("stacks.create.errors.lost");
    if (err.status === 403) return t("stacks.create.errors.permission");
    if (err.status === 422 || err.status === 404) return t("gitSource.errors.createFailed");
    return t("stacks.create.errors.generic");
  }

  function submitConvert() {
    if (!convert || !ready || !adminNeeds.allowed || connect.isPending || folder === null) return;
    setConnectError(null);
    connect.mutate(
      {
        draftVersion: convert.draft.version,
        repoId,
        refKind: kind,
        ref: gitRef,
        folder,
        composeFiles: files,
        files: leftover.map((f) => ({
          path: f.path,
          keep: choiceOf(f, choices, repoPathSet) === "keep",
        })),
        ...(sentVars.length > 0 ? { vars: sentVars } : {}),
        ...(overlays.length > 0 ? { overlays } : {}),
      },
      {
        onSuccess: () => {
          finished.current = true;
          setTable([]);
          setOverlays([]);
          connect.reset();
          // The editor and the review read the draft again, not the one this page read.
          queryClient.removeQueries({ queryKey: draftKey(envId, convert.stackId) });
          void navigate({
            to: "/env/$envId/stacks/$stackId/review",
            params: { envId, stackId: convert.stackId },
          });
        },
        onError: (err) => setConnectError(err),
      },
    );
  }

  function submit() {
    if (convert) return submitConvert();
    if (!ready || create.isPending || folder === null) return;
    setRefusal(null);
    // What the last check read, else what the top of the page resolved the ref to.
    const checked = check.check?.commit.sha ?? commit;
    create.mutate(
      {
        name,
        repoId,
        refKind: kind,
        ref: gitRef,
        folder,
        composeFiles: files,
        // The commit the person was shown: a ref that has moved since is refused, not followed.
        ...(checked && FULL_COMMIT.test(checked) ? { commit: checked } : {}),
        ...(sentVars.length > 0 ? { vars: sentVars } : {}),
        ...(overlays.length > 0 ? { overlays } : {}),
      },
      {
        onSuccess: (made) => {
          finished.current = true;
          setTable([]);
          setOverlays([]);
          create.reset();
          toastSuccess(t("stacks.create.success", { name }));
          void navigate({
            to: "/env/$envId/stacks/$stackId",
            params: { envId, stackId: made.stackId },
            replace: true,
          });
        },
        onError: (err) => {
          setRefusal(refusalText(err));
          if (err.type === "commit_moved") {
            // Read the ref again: the new head becomes the commit of the next check.
            void queryClient.invalidateQueries({
              queryKey: gitTreeQuery(envId, repoId, kind, gitRef, "").queryKey.slice(0, -1),
            });
          }
        },
      },
    );
  }

  const pending = convert ? connect.isPending : create.isPending;
  const createButton = (
    <Button
      type="button"
      className="max-md:h-11"
      aria-disabled={!ready || (convert && !adminNeeds.allowed) || undefined}
      aria-busy={pending || undefined}
      onClick={submit}
    >
      {pending ? <Spinner aria-hidden="true" /> : null}
      {convert
        ? t(pending ? "gitSource.convert.footer.pending" : "gitSource.convert.footer.submit")
        : t(pending ? "gitSource.footer.pending" : "gitSource.footer.create")}
    </Button>
  );
  const conflict = connectError?.status === 409 && connectError.type === "draft_changed";
  const conflictUser = connectError?.fields.find((f) => f.location === "savedBy")?.message ?? "";

  return (
    <div className="flex flex-col gap-6">
      <div className="flex w-full max-w-6xl flex-col gap-6">
        <header className="flex flex-col gap-2">
          <Breadcrumb envId={envId} convert={convert?.stackId} name={name} />
          <h2 className="line-clamp-2 min-w-0 break-all text-2xl font-semibold">
            {t(convert ? "gitSource.convert.title" : "gitSource.page.title", { name })}
          </h2>
          <p className="max-w-prose text-sm text-muted-foreground">
            {t(convert ? "gitSource.convert.body" : "gitSource.page.body", { name })}
          </p>
          {target && !convert ? (
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
        {connectError ? (
          <Alert variant="destructive" role="alert">
            <CircleX aria-hidden="true" />
            <AlertDescription className="flex flex-wrap items-center gap-3">
              <span className="break-words">
                {conflict
                  ? conflictUser
                    ? t("gitSource.convert.errors.conflict", { user: conflictUser, name })
                    : t("gitSource.convert.errors.conflictUnknown", { name })
                  : connectRefusal(connectError)}
              </span>
              {conflict ? (
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="max-md:h-11"
                  onClick={reload}
                >
                  {t("gitSource.convert.errors.reload")}
                </Button>
              ) : null}
            </AlertDescription>
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
              {request ? (
                <CheckSummary state={check} repoName={repo.name} onAddOverlay={setDialogPath} />
              ) : null}
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

        {convert ? (
          <LeftoverFilesCard
            name={name}
            files={leftover}
            repoPaths={repoPathSet}
            choices={choices}
            onChoose={(path, choice: LeftoverChoice) =>
              setChoices((all) => ({ ...all, [path]: choice }))
            }
          />
        ) : null}

        <OverlayFilesCard
          missing={check.check?.missing ?? []}
          overlays={addedPaths}
          problems={check.check?.problems ?? []}
          onAdd={setDialogPath}
          onRemove={(path) => setOverlays((all) => all.filter((o) => o.path !== path))}
        />

        <RequiredVariablesLine vars={check.check?.vars ?? null} />
      </div>

      <EditorFooter>
        <p className="me-auto min-w-0 text-sm break-words text-muted-foreground max-md:hidden">
          {convert
            ? t("gitSource.convert.footer.note", { env: envName })
            : t("gitSource.footer.note")}
        </p>
        <Button asChild variant="outline" className="max-md:h-11">
          {convert ? (
            <Link to="/env/$envId/stacks/$stackId" params={{ envId, stackId: convert.stackId }}>
              {t("gitSource.footer.cancel")}
            </Link>
          ) : (
            <Link to="/env/$envId/stacks" params={{ envId }}>
              {t("gitSource.footer.cancel")}
            </Link>
          )}
        </Button>
        <NeedsTooltip
          reason={
            convert && !adminNeeds.allowed
              ? (adminNeeds.reason ?? t("gitSource.convert.errors.permission", { env: envName }))
              : ready
                ? undefined
                : t("gitSource.footer.needsAll")
          }
        >
          {createButton}
        </NeedsTooltip>
      </EditorFooter>

      {dialogPath !== null ? (
        <AddOverlayDialog
          existingRepoPaths={repoPaths}
          overlays={overlayPaths}
          initialPath={dialogPath}
          onAdd={(overlay) => setOverlays((all) => [...all, overlay])}
          onClose={() => setDialogPath(null)}
        />
      ) : null}

      {adding ? (
        <RepositorySheet
          mode="add"
          defaultScope={envId}
          onSaved={(saved) => chooseRepo(saved.id)}
          onClose={() => setAdding(false)}
        />
      ) : null}

      <AlertDialog
        open={blocker.status === "blocked"}
        onOpenChange={(next) => {
          if (!next) blocker.reset?.();
        }}
      >
        <AlertDialogContent className="sm:max-w-120">
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(convert ? "gitSource.convert.guard.title" : "gitSource.guard.title")}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(convert ? "gitSource.convert.guard.body" : "gitSource.guard.body", { name })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel autoFocus>
              {t(convert ? "gitSource.convert.guard.keep" : "gitSource.guard.keep")}
            </AlertDialogCancel>
            <AlertDialogAction onClick={() => blocker.proceed?.()}>
              {t(convert ? "gitSource.convert.guard.discard" : "gitSource.guard.discard")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

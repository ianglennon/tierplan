// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { CircleX } from "lucide-react";
import { useId, useState } from "react";

import { type GitRefKind, type GitRequestError, gitEnvReposQuery } from "@/api/git";
import { type StackGit, stackGitKey, useChangeStackGitSource } from "@/api/stack-git";
import { draftKey, draftQuery, invalidateStacks } from "@/api/stacks";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Spinner } from "@/components/ui/spinner";
import { useBrowseReason } from "@/features/git-source/browse-reason";
import { ComposeFileList } from "@/features/git-source/compose-file-list";
import { FolderBrowser } from "@/features/git-source/folder-browser";
import { refIsChosen } from "@/features/git-source/ref-picker";
import { RepoCard } from "@/features/git-source/repo-card";
import { useEnvName } from "@/features/resources/use-env-name";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface SourceCardProps {
  envId: string;
  stackId: string;
  stackName: string;
  git: StackGit;
}

// SourceCard is the Source card of the Git tab (UI-SPEC "Git tab", D-11, D-12): where the stack comes from, as it is
// applied now, and "Change source…", which turns the card into the pickers of the From git page. Nothing is applied by
// the card: "Review change" saves the new source as the stack's draft and opens the Review, and only a good deploy of
// that draft makes it the stack's source.
export function SourceCard({ envId, stackId, stackName, git }: SourceCardProps) {
  const t = useT();
  const headingId = useId();
  const needs = useNeeds("git.manage", envId);
  const [editing, setEditing] = useState(false);

  return (
    <section aria-labelledby={headingId}>
      <Card className="gap-4 [--card-spacing:--spacing(4)] md:[--card-spacing:--spacing(6)]">
        <CardHeader>
          <CardTitle>
            <h3 id={headingId} className="text-base font-semibold">
              {t("stackGit.source.title")}
            </h3>
          </CardTitle>
          <CardDescription>{t("stackGit.source.body", { name: stackName })}</CardDescription>
        </CardHeader>
        <CardContent className="flex min-w-0 flex-col gap-4">
          {editing ? (
            <SourceEditor
              envId={envId}
              stackId={stackId}
              stackName={stackName}
              git={git}
              onClose={() => setEditing(false)}
            />
          ) : (
            <>
              <SourceRows git={git} />
              {needs.allowed || needs.reason ? (
                <div>
                  <NeedsTooltip reason={needs.allowed ? undefined : needs.reason}>
                    <Button
                      type="button"
                      variant="outline"
                      className="max-md:h-11"
                      aria-disabled={!needs.allowed || undefined}
                      onClick={needs.allowed ? () => setEditing(true) : undefined}
                    >
                      {t("stackGit.source.change")}
                    </Button>
                  </NeedsTooltip>
                </div>
              ) : null}
            </>
          )}
        </CardContent>
      </Card>
    </section>
  );
}

// The rows of the Overview's Details, in the card: the repository, the ref and what kind it is, the folder and the
// compose files in the order they are merged. Every value is the server's and is drawn as text.
function SourceRows({ git }: { git: StackGit }) {
  const t = useT();
  const files = git.composeFiles ?? [];
  const row = "grid gap-1 sm:grid-cols-[10rem_1fr] sm:gap-4";
  return (
    <dl className="flex flex-col gap-3 text-sm">
      <div className={row}>
        <dt className="text-muted-foreground">{t("stackGit.details.repository")}</dt>
        <dd className="min-w-0 break-words">{git.repoName || t("stackGit.strip.repoUnknown")}</dd>
      </div>
      <div className={row}>
        <dt className="text-muted-foreground">{t("stackGit.details.ref")}</dt>
        <dd className="min-w-0">
          <code className="font-mono text-xs break-all">{git.ref}</code>{" "}
          <span className="text-muted-foreground">{t(`stackGit.kind.${git.refKind}`)}</span>
        </dd>
      </div>
      <div className={row}>
        <dt className="text-muted-foreground">{t("stackGit.details.folder")}</dt>
        <dd className="min-w-0">
          {git.folder ? (
            <code className="font-mono text-xs break-all">{git.folder}</code>
          ) : (
            t("stackGit.details.rootFolder")
          )}
        </dd>
      </div>
      <div className={row}>
        <dt className="text-muted-foreground">{t("stackGit.details.composeFiles")}</dt>
        <dd className="min-w-0">
          <ol className="flex flex-col">
            {files.map((f) => (
              <li key={f}>
                <code className="font-mono text-xs break-all">{f}</code>
              </li>
            ))}
          </ol>
        </dd>
      </div>
    </dl>
  );
}

interface SourceEditorProps extends SourceCardProps {
  onClose: () => void;
}

// SourceEditor is the card in edit mode: the repository and ref, the folder and its compose files, started from what the
// stack follows now. The draft is read once when editing begins and its version is what "Review change" sends, so a draft
// someone saved since is refused and the person told to reload, not overwritten.
function SourceEditor({ envId, stackId, stackName, git, onClose }: SourceEditorProps) {
  const t = useT();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const envName = useEnvName(envId);
  const reason = useBrowseReason();
  const repos = useQuery(gitEnvReposQuery(envId));
  const draft = useQuery(draftQuery(envId, stackId));
  const change = useChangeStackGitSource(envId, stackId);

  const [repoId, setRepoId] = useState(git.repoId);
  const [kind, setKind] = useState<GitRefKind>(git.refKind);
  const [gitRef, setGitRef] = useState(git.ref);
  // The folder chosen as the stack's root: null while none is, "" for the repository root.
  const [folder, setFolder] = useState<string | null>(git.folder);
  const [files, setFiles] = useState<string[]>(git.composeFiles ?? []);
  const [refusal, setRefusal] = useState<GitRequestError | null>(null);

  const list = repos.data ?? [];
  const repo = list.find((r) => r.id === repoId);
  const ready =
    repo !== undefined && refIsChosen(kind, gitRef) && folder !== null && files.length > 0;
  const version = draft.data?.version;

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

  function submit() {
    if (!ready || version === undefined || folder === null || change.isPending) return;
    setRefusal(null);
    change.mutate(
      { draftVersion: version, repoId, refKind: kind, ref: gitRef, folder, composeFiles: files },
      {
        onSuccess: () => {
          // The editor reads the draft again, not the one this card read.
          queryClient.removeQueries({ queryKey: draftKey(envId, stackId) });
          void navigate({
            to: "/env/$envId/stacks/$stackId/review",
            params: { envId, stackId },
          });
        },
        onError: (err) => setRefusal(err),
      },
    );
  }

  // The draft moved while the person was choosing: read what is there now, and start again from the stack's own source.
  function reload() {
    queryClient.removeQueries({ queryKey: draftKey(envId, stackId) });
    void queryClient.invalidateQueries({ queryKey: stackGitKey(envId, stackId) });
    invalidateStacks(queryClient, envId);
    onClose();
  }

  const conflict = refusal?.status === 409 && refusal.type === "draft_changed";
  const needsReason = ready
    ? version === undefined
      ? t("stackGit.source.reading")
      : undefined
    : t("stackGit.source.needsAll");
  const blocked = needsReason !== undefined;

  return (
    <div className="flex flex-col gap-4">
      {refusal ? (
        <Alert variant="destructive" role="alert">
          <CircleX aria-hidden="true" />
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span className="break-words">
              {conflict
                ? conflictLine(t, stackName, refusal)
                : t("stackGit.source.failed", {
                    reason: failureLine(t, reason, repo?.name, refusal),
                  })}
            </span>
            {conflict ? (
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="max-md:h-11"
                onClick={reload}
              >
                {t("stackGit.source.reload")}
              </Button>
            ) : null}
          </AlertDescription>
        </Alert>
      ) : null}
      {draft.isError ? (
        <Alert variant="destructive" role="alert">
          <CircleX aria-hidden="true" />
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("stackGit.source.draftFailed")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={draft.isFetching}
              onClick={() => void draft.refetch()}
            >
              {t("stackGit.source.reload")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}

      <RepoCard
        envId={envId}
        repos={list}
        repoId={repoId}
        onRepoChange={chooseRepo}
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
      {repo && refIsChosen(kind, gitRef) ? (
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
        </>
      ) : null}

      <div className="flex flex-wrap justify-end gap-2">
        <Button
          type="button"
          variant="outline"
          className={TOUCH_HEIGHT}
          disabled={change.isPending}
          onClick={onClose}
        >
          {t("stackGit.source.cancel")}
        </Button>
        <NeedsTooltip reason={needsReason}>
          <Button
            type="button"
            className={TOUCH_HEIGHT}
            aria-disabled={blocked || undefined}
            aria-busy={change.isPending || undefined}
            onClick={submit}
          >
            {change.isPending ? <Spinner aria-hidden="true" /> : null}
            {change.isPending ? t("stackGit.source.preparing") : t("stackGit.source.review")}
          </Button>
        </NeedsTooltip>
      </div>
    </div>
  );
}

type Translate = (key: string, options?: Record<string, unknown>) => string;

// Who saved the draft, as the 409 names them; the line does not guess when the server did not.
function conflictLine(t: Translate, name: string, err: GitRequestError): string {
  const user = err.fields.find((f) => f.location === "savedBy")?.message ?? "";
  return user
    ? t("stackGit.source.conflict", { user, name })
    : t("stackGit.source.conflictUnknown", { name });
}

// Why the source could not be changed, in words: the git failures as browsing words them, the rest as their own lines.
function failureLine(
  t: Translate,
  reason: (err: GitRequestError, repo: string) => string,
  repo: string | undefined,
  err: GitRequestError,
): string {
  if (err.type === "repository_not_found") return t("stackGit.source.repoGone");
  if (err.status === 403) return t("stackGit.source.permission");
  if (err.status === 0 || err.status === 422) return reason(err, repo ?? "");
  return t("stackGit.source.other");
}

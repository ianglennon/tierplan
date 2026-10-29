// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CircleX, Info } from "lucide-react";
import { useId, useState } from "react";

import { useRestoreStack } from "@/api/stack-drift";
import { useRollbackToRevision } from "@/api/stack-history";
import {
  type DeployRequestError,
  type ReviewMode,
  type ReviewRequestError,
  reviewQuery,
  type StackReview,
  stackQuery,
  useDeployStack,
} from "@/api/stacks";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useEnvName } from "@/features/resources/use-env-name";
import { EditorFooter } from "@/features/service-editor/editor-footer";
import { shortSha } from "@/features/stack-git/sha-link";
import { BlockingList } from "@/features/stack-review/blocking-list";
import { CommitsCard } from "@/features/stack-review/commits-card";
import { ConcurrentDeployPanel } from "@/features/stack-review/concurrent-deploy-panel";
import { ImagePinTable } from "@/features/stack-review/image-pin-table";
import { OtherResourcesTable } from "@/features/stack-review/other-resources-table";
import { RevertsDriftSection } from "@/features/stack-review/reverts-drift-section";
import { ReviewSummary } from "@/features/stack-review/review-summary";
import { ServiceDiffList } from "@/features/stack-review/service-diff-list";
import { WarningsList } from "@/features/stack-review/warnings-list";
import { YamlDiff } from "@/features/stack-review/yaml-diff";
import { useT } from "@/i18n/use-t";
import { toastInfo } from "@/lib/toast";

type Translate = (key: string, options?: Record<string, unknown>) => string;

interface ReviewPageProps {
  envId: string;
  stackId: string;
  // What is reviewed: the saved draft (deploy), a fresh digest for the named services (update), an earlier revision
  // (rollback) or the deployed revision again (restore).
  mode?: ReviewMode;
  services?: string[];
  // For a roll back, the revision it restores.
  revision?: number;
  // What to do once the server has started the deploy: the route replaces this page with the
  // deploy's console, so Back returns to the editor and not to a finished review.
  onDeployed: (jobId: string) => void;
}

// deployRefusal words a deploy the server refused. The server decides every refusal; this only
// says what it means in the page's words (T-06-60). A draft or digests that moved since the
// review are the same advice: look again.
function deployRefusal(t: Translate, err: DeployRequestError, envName: string): string {
  switch (err.type) {
    case "operation_locked":
      return t("stackConverge.lock.refused", { env: envName });
    case "draft_changed":
      return t("stackReview.deployErrors.draftChanged");
    case "pins_invalid":
      return t("stackReview.deployErrors.pinsInvalid");
    case "services_invalid":
      return t("stackUpdate.refusal.services_invalid");
    case "concurrent_unacknowledged":
      return t("stackYamlDiff.concurrent.refused");
    case "privileged_compose":
      return t("stackReview.deployErrors.needsAdmin");
    case "stack_name_clash":
      return t("stackReview.deployErrors.nameClash");
    case "target_changed":
    case "target_engine_missing":
      return t("stackReview.deployErrors.targetChanged");
    case "too_many_deploys":
      return t("stackReview.deployErrors.tooMany");
  }
  if (err.status === 0) return t("stackReview.deployErrors.lost");
  if (err.status === 403) return t("stackReview.deployErrors.permission");
  return t("stackReview.deployErrors.generic");
}

// rollbackRefusal words a roll back the server refused: the answers only a roll back gives, then the ones it
// shares with a deploy.
function rollbackRefusal(t: Translate, err: DeployRequestError, envName: string): string {
  switch (err.type) {
    case "deployed_changed":
      return t("stackRollback.review.errors.deployedChanged");
    case "revision_not_good":
      return t("stackRollback.review.errors.notGood");
    case "revision_not_found":
      return t("stackRollback.review.errors.gone");
    case "privileged_compose":
      return t("stackRollback.review.errors.needsAdmin");
    case "stack_invalid":
    case "compose_invalid":
    case "compose_unsupported":
    case "pins_invalid":
      return t("stackRollback.review.errors.invalid");
  }
  if (err.status === 0) return t("stackRollback.review.errors.lost");
  const shared = deployRefusal(t, err, envName);
  return shared === t("stackReview.deployErrors.generic")
    ? t("stackRollback.review.errors.generic")
    : shared;
}

// restoreRefusal words a restore the server refused: the answers only a restore gives, then the ones it shares with
// a deploy.
function restoreRefusal(t: Translate, err: DeployRequestError, envName: string): string {
  switch (err.type) {
    case "deployed_changed":
      return t("stackRestore.review.errors.deployedChanged");
    case "no_deployed_revision":
      return t("stackRestore.review.errors.none");
    case "privileged_compose":
      return t("stackRestore.review.errors.needsAdmin");
    case "stack_invalid":
    case "compose_invalid":
    case "compose_unsupported":
    case "pins_invalid":
      return t("stackRestore.review.errors.invalid");
  }
  if (err.status === 0) return t("stackRestore.review.errors.lost");
  const shared = deployRefusal(t, err, envName);
  return shared === t("stackReview.deployErrors.generic")
    ? t("stackRestore.review.errors.generic")
    : shared;
}

// The review's code for the blocking problem of an environment that is busy. It has nothing in the
// files to fix, so it is shown as the operation lock and not among the errors.
const OPERATION_LOCKED = "operation_locked";

// listOf joins the kinds of host access into one phrase: "bind mounts and host networking".
function listOf(items: string[]): string {
  return new Intl.ListFormat("en", { style: "long", type: "conjunction" }).format(items);
}

// noChange says whether a deploy of the review would do nothing: no service to add, change or
// remove, and no network, secret or config but the external ones the stack only names.
function noChange(data: StackReview): boolean {
  return (
    (data.services ?? []).length === 0 && (data.objects ?? []).every((o) => o.change === "external")
  );
}

// deployReason is why Deploy stack is off, in the order that matters: host access the caller may
// not deploy, an image the registry cannot be asked about, errors, a stack with nothing to change,
// and a deployment made after the draft was started that the person has not acknowledged.
// Warnings are never a reason (D-10). Absent when a deploy may start. The server refuses
// every one of these again at the deploy; this only says why the button waits. A restore with nothing
// to change has no drift to put back.
function deployReason(
  t: Translate,
  data: StackReview,
  acknowledged: boolean,
  locked: boolean,
  envName: string,
  restore: boolean,
): string | undefined {
  // Another operation holds the environment: nothing can start until it ends, whatever else is true.
  if (locked) return t("stackConverge.lock.reason", { env: envName });
  const kinds = data.needsAdmin ?? [];
  if (kinds.length > 0) return t("stackReview.reasons.needsAdmin", { kinds: listOf(kinds) });
  const lost = (data.images ?? []).find(
    (i) => i.state === "unreachable" || i.state === "unresolved",
  );
  if (lost) return t("stackReview.reasons.lookup", { ref: lost.image });
  const errors = (data.problems ?? []).filter(
    (p) => p.severity === "error" && p.code !== OPERATION_LOCKED,
  ).length;
  if (errors > 0) return t("stackReview.reasons.errors", { count: errors });
  if (noChange(data)) {
    return t(restore ? "stackRestore.review.reasons.nothing" : "stackReview.reasons.nothing");
  }
  // A deployment made since the draft was started needs the person to say they saw it (D-09).
  if (data.concurrent && !acknowledged) return t("stackYamlDiff.concurrent.reason");
  return undefined;
}

// loadRefusal words a review the server would not make.
function loadRefusal(t: Translate, err: Error | null): string {
  const type = (err as ReviewRequestError | null)?.type;
  if (type === "target_changed" || type === "target_engine_missing") {
    return t("stackReview.error.targetChanged");
  }
  if (type === "services_invalid") return t("stackUpdate.refusal.services_invalid");
  if (type === "no_deployed_revision") return t("stackRestore.review.error.none");
  if (type === "revision_not_good") return t("stackRollback.review.error.notGood");
  if (type === "revision_not_found") return t("stackRollback.review.error.gone");
  return t("stackReview.error.body");
}

// ReviewPage is the second step of a deploy (STK-06, D-08, D-10, D-11): what the deploy would
// change against what is running, field by field, and the images it would run. Nothing deploys
// that the person did not see: Deploy stack sends the version of the draft and the digests this
// page was made of, and the server refuses a draft that moved since. The page is capped at
// 1152px; every value is text.
export function ReviewPage({
  envId,
  stackId,
  mode = "deploy",
  services = [],
  revision,
  onDeployed,
}: ReviewPageProps) {
  const t = useT();
  const envName = useEnvName(envId);
  const stack = useQuery(stackQuery(envId, stackId));
  const review = useQuery(reviewQuery(envId, stackId, mode, services, revision));
  const deploy$ = useDeployStack(envId, stackId);
  const rollback$ = useRollbackToRevision(envId, stackId);
  const restore$ = useRestoreStack(envId, stackId);
  const [deployError, setDeployError] = useState<string | null>(null);
  const [tab, setTab] = useState<string | null>(null);
  const [acknowledged, setAcknowledged] = useState(false);
  // The roll back was refused because the stack was deployed again: the review is out of date and says so.
  const [stale, setStale] = useState(false);
  const name = stack.data?.name;
  const data = review.data;
  const update = mode === "update";
  const rollback = mode === "rollback" && revision !== undefined;
  const restore = mode === "restore";
  // A roll back and a restore are both the exact redeploy of a stored revision: its digests, no registry. The revision a
  // restore redeploys is the deployed one, which the review names.
  const exact = rollback || restore;
  const seq = restore ? data?.revision : revision;
  // The review of the draft of a git stack says which commit it deploys (D-17). The title waits for nothing: the stack
  // knows it is from git; the body and the commit line wait for the review, which names the commit.
  const gitStack = mode === "deploy" && !!stack.data?.git;
  const gitReview = mode === "deploy" ? data?.git : undefined;

  // Roll back stack sends the revision that was reviewed and the deployed revision the review was made against,
  // so the server refuses a stack that was deployed again since. Like a deploy, it is pending after the first
  // click and the request is never repeated.
  function rollBack() {
    if (!data || revision === undefined || rollback$.isPending || rollback$.isSuccess) return;
    setDeployError(null);
    rollback$.mutate(
      { revision, ...(data.deployedSeq > 0 ? { expectedDeployed: data.deployedSeq } : {}) },
      {
        onSuccess: (answer) => onDeployed(answer.jobId),
        onError: (err) => {
          setDeployError(rollbackRefusal(t, err, envName));
          setStale(err.type === "deployed_changed");
        },
      },
    );
  }

  // Restore stack sends the deployed revision that was reviewed, and the server refuses it when the stack was deployed
  // again since. Like a deploy, it is pending after the first click and the request is never repeated: a restore only
  // ever starts from this click (D-23).
  function restoreStack() {
    if (!data || seq === undefined || restore$.isPending || restore$.isSuccess) return;
    setDeployError(null);
    restore$.mutate(
      { revision: seq },
      {
        onSuccess: (answer) => onDeployed(answer.jobId),
        onError: (err) => {
          setDeployError(restoreRefusal(t, err, envName));
          setStale(err.type === "deployed_changed");
        },
      },
    );
  }

  // Deploy sends what was reviewed and nothing else. The button is pending after the first click
  // and the request is never repeated (T-07-73).
  function deploy() {
    if (!data || deploy$.isPending || deploy$.isSuccess) return;
    setDeployError(null);
    deploy$.mutate(
      {
        draftVersion: data.draftVersion,
        pins: data.pins,
        mode: update ? "update" : "deploy",
        ...(update ? { services } : {}),
        // Sent only when the review said a deployment was made since, and the box was ticked.
        ...(data.concurrent && acknowledged ? { acknowledgeConcurrent: true } : {}),
      },
      {
        onSuccess: (answer) => onDeployed(answer.jobId),
        onError: (err) => setDeployError(deployRefusal(t, err, envName)),
      },
    );
  }

  // The lock shows in the stack's own answer and in the review's problems; either holds Deploy back.
  const locked =
    Boolean(stack.data?.lock) || (data?.problems ?? []).some((p) => p.code === OPERATION_LOCKED);
  const reason = data ? deployReason(t, data, acknowledged, locked, envName, restore) : undefined;
  const blocked = !data || reason !== undefined;
  const starting = restore
    ? restore$.isPending || restore$.isSuccess
    : rollback
      ? rollback$.isPending || rollback$.isSuccess
      : deploy$.isPending || deploy$.isSuccess;
  const reasonId = useId();
  const problems = (data?.problems ?? []).filter((p) => p.code !== OPERATION_LOCKED);
  const changed = data?.services ?? [];
  const unchanged = data !== undefined && noChange(data);
  const yaml = data?.yaml;
  const yamlChanged = yaml?.changedLines ?? 0;
  // Changes (against what is running) is the page's first tab, unless there is nothing to show
  // there and the text still differs from the last deploy.
  const shown = tab ?? (unchanged && yamlChanged > 0 ? "yaml" : "changes");
  const firstDeploy = !exact && changed.length > 0 && changed.every((s) => s.change === "added");
  const failed = review.isError && !data;
  // A standalone host recreates containers; a swarm, services (the group and the warning word it).
  const ownershipContext: { context?: "standalone" } =
    stack.data?.target === "standalone" ? { context: "standalone" } : {};

  // Pressing Deploy stack while it is off says why, for a touch screen that has no hover.
  function pressed() {
    if (reason) toastInfo(reason);
  }

  const deployButton = (
    <Button
      type="button"
      className="max-md:h-11"
      aria-disabled={blocked || starting ? true : undefined}
      aria-describedby={reason ? reasonId : undefined}
      onClick={
        blocked || starting ? pressed : restore ? restoreStack : rollback ? rollBack : deploy
      }
    >
      {starting ? (
        <>
          <Spinner aria-hidden="true" />
          {t(
            restore
              ? "stackRestore.review.footer.starting"
              : rollback
                ? "stackRollback.review.footer.starting"
                : "stackReview.footer.starting",
          )}
        </>
      ) : (
        t(
          restore
            ? "stackRestore.review.footer.restore"
            : rollback
              ? "stackRollback.review.footer.rollback"
              : "stackReview.footer.deploy",
        )
      )}
    </Button>
  );

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6">
      <div className="flex flex-col gap-1">
        {name === undefined ? (
          <Skeleton className="h-7 w-64" />
        ) : (
          <h3 className="text-xl font-semibold break-all">
            {restore
              ? t("stackRestore.review.title", { name })
              : rollback
                ? t("stackRollback.review.title", { name, seq: revision })
                : update
                  ? t("stackReview.titleUpdate", { name })
                  : gitStack
                    ? t("stackReview.titleGit", { name })
                    : t("stackReview.title", { name })}
          </h3>
        )}
        <p className="text-sm text-muted-foreground">
          {restore
            ? seq === undefined
              ? t("stackRestore.review.bodyPending")
              : t("stackRestore.review.body", { seq })
            : rollback
              ? t("stackRollback.review.body", { seq: revision })
              : update
                ? t("stackReview.bodyUpdate")
                : gitReview
                  ? t("stackReview.bodyGit", {
                      ref: gitReview.refKind === "commit" ? shortSha(gitReview.ref) : gitReview.ref,
                      folder: gitReview.folder || t("stackGit.status.rootFolder"),
                      sha: shortSha(gitReview.commit),
                      env: envName,
                    })
                  : t("stackReview.body", { env: envName })}
        </p>
        {data || failed ? null : (
          <p role="status" className="text-sm text-muted-foreground">
            {t("stackReview.loading")}
          </p>
        )}
      </div>
      {failed ? (
        <Alert variant="destructive">
          <CircleX aria-hidden="true" />
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("stackReview.error.title")}</span>
            <span>{loadRefusal(t, review.error)}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={review.isFetching}
              onClick={() => void review.refetch()}
            >
              {t("stackReview.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : (
        <>
          {data?.concurrent ? (
            <ConcurrentDeployPanel
              concurrent={data.concurrent}
              acknowledged={acknowledged}
              onAcknowledge={setAcknowledged}
            />
          ) : null}
          {rollback && stack.data?.draftSavedAt ? (
            <Alert role="status">
              <Info aria-hidden="true" />
              <AlertDescription>{t("stackRollback.review.draftNotice")}</AlertDescription>
            </Alert>
          ) : null}
          <BlockingList problems={problems} envId={envId} stackId={stackId} />
          <WarningsList problems={problems} envId={envId} stackId={stackId} />
          {firstDeploy ? (
            <Alert role="status">
              <Info aria-hidden="true" />
              <AlertTitle>{t("stackReview.firstDeploy.title")}</AlertTitle>
              <AlertDescription>{t("stackReview.firstDeploy.body")}</AlertDescription>
            </Alert>
          ) : null}
          {data?.adoption?.pending && data.summary.ownershipOnly > 0 ? (
            <Alert role="status">
              <Info aria-hidden="true" />
              <AlertTitle>
                {t("stackOwnership.restart.title", {
                  count: data.summary.ownershipOnly,
                  ...ownershipContext,
                })}
              </AlertTitle>
              <AlertDescription>
                {t("stackOwnership.restart.body", ownershipContext)}
              </AlertDescription>
            </Alert>
          ) : null}
          {data && !restore ? (
            <RevertsDriftSection
              reverts={data.revertsDrift ?? []}
              unavailable={data.revertsDriftUnavailable}
              mode={rollback ? "rollback" : update ? "update" : "deploy"}
            />
          ) : null}
          <ReviewSummary summary={data?.summary} git={gitReview} />
          {gitReview && gitReview.deployedCommit !== "" && !gitReview.sourceChanged ? (
            <CommitsCard
              envId={envId}
              stackId={stackId}
              head={gitReview.commit}
              folder={gitReview.folder}
              repoHost={stack.data?.git?.repoHost ?? ""}
              repoPath={stack.data?.git?.repoPath ?? ""}
            />
          ) : null}
          <ImagePinTable
            images={data ? (data.images ?? []) : undefined}
            mode={mode}
            revision={seq}
          />
          {unchanged ? (
            <Empty className="border">
              <EmptyHeader>
                <EmptyTitle role="heading" aria-level={3}>
                  {t("stackReview.noChanges.title")}
                </EmptyTitle>
                <EmptyDescription>
                  {restore
                    ? t("stackRestore.review.noChanges.body", { seq })
                    : rollback
                      ? t("stackRollback.review.noChanges.body", { seq: revision })
                      : update
                        ? t("stackReview.noChanges.bodyUpdate")
                        : t("stackReview.noChanges.body")}
                </EmptyDescription>
              </EmptyHeader>
            </Empty>
          ) : null}
          {data && (!unchanged || yamlChanged > 0) ? (
            <Tabs value={shown} onValueChange={setTab} className="gap-4">
              <TabsList variant="line" aria-label={t("stackReview.changes.label")} className="h-9">
                <TabsTrigger value="changes" className="max-md:h-11">
                  {t("stackReview.changes.tab")}
                </TabsTrigger>
                <TabsTrigger value="yaml" className="max-md:h-11">
                  {yamlChanged > 0
                    ? t("stackYamlDiff.tabCount", { count: yamlChanged })
                    : t("stackYamlDiff.tab")}
                </TabsTrigger>
              </TabsList>
              <TabsContent value="changes" className="flex flex-col gap-4">
                <ServiceDiffList services={changed} context={ownershipContext.context} />
                <OtherResourcesTable objects={data.objects ?? []} />
              </TabsContent>
              <TabsContent value="yaml">
                <YamlDiff
                  hunks={yaml?.hunks}
                  lines={yaml?.lines}
                  caption={
                    restore
                      ? t("stackRestore.review.yamlCaption", { seq })
                      : rollback
                        ? t("stackRollback.review.yamlCaption", {
                            seq: revision,
                            deployed: data.deployedSeq,
                          })
                        : t("stackYamlDiff.caption")
                  }
                />
              </TabsContent>
            </Tabs>
          ) : null}
        </>
      )}
      {deployError ? (
        <Alert variant="destructive">
          <CircleX aria-hidden="true" />
          <AlertDescription className="flex flex-wrap items-center gap-3 break-words">
            <span>{deployError}</span>
            {stale ? (
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="max-md:h-11"
                disabled={review.isFetching}
                onClick={() => {
                  setDeployError(null);
                  setStale(false);
                  void review.refetch();
                }}
              >
                {t(
                  restore
                    ? "stackRestore.review.errors.reviewAgain"
                    : "stackRollback.review.errors.reviewAgain",
                )}
              </Button>
            ) : null}
          </AlertDescription>
        </Alert>
      ) : null}
      <EditorFooter>
        {reason ? (
          <p id={reasonId} className="me-auto min-w-0 text-sm break-words text-muted-foreground">
            {reason}
          </p>
        ) : null}
        <Button asChild variant="outline" className="max-md:h-11">
          {restore ? (
            <Link to="/env/$envId/stacks/$stackId/drift" params={{ envId, stackId }}>
              {t("stackRestore.review.footer.back")}
            </Link>
          ) : rollback ? (
            <Link
              to="/env/$envId/stacks/$stackId/history"
              params={{ envId, stackId }}
              search={{ rev: revision }}
            >
              {t("stackRollback.review.footer.back")}
            </Link>
          ) : (
            <Link to="/env/$envId/stacks/$stackId/editor" params={{ envId, stackId }}>
              {t("stackReview.footer.back")}
            </Link>
          )}
        </Button>
        {reason ? (
          <Tooltip>
            <TooltipTrigger asChild>{deployButton}</TooltipTrigger>
            <TooltipContent>{reason}</TooltipContent>
          </Tooltip>
        ) : (
          deployButton
        )}
      </EditorFooter>
    </div>
  );
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CirclePause, Info } from "lucide-react";
import { useState } from "react";

import { environmentsQuery } from "@/api/environments";
import { stackGitQuery } from "@/api/stack-git";
import type { StackDetail } from "@/api/stacks";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { AutoDeployConfirmDialog } from "@/features/stack-git/auto-deploy-confirm-dialog";
import { shortSha } from "@/features/stack-git/sha-link";
import { usePullAndReview } from "@/features/stacks/stack-actions";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface AutoDeployPausedAlertProps {
  stack: StackDetail;
}

// AutoDeployPausedAlert is the page's notice that a git stack's automatic deploys are paused (UI-SPEC "Stack page (git
// stack)", D-18, D-22). It is state and not a notification, so it has no dismiss, and a stack shows at most one of the
// sync error and this alert (the error wins). Two variants: a pause after a roll back (an info alert with the pending
// head, Review update and Turn on auto-deploy…, until auto-deploy is turned on again) and a pause because the owner no
// longer holds the permission to deploy. Every name is drawn as text.
export function AutoDeployPausedAlert({ stack }: AutoDeployPausedAlertProps) {
  const git = stack.git;
  if (git?.autoDeployPaused && git.autoDeployPausedReason === "rollback") {
    return git.status === "error" ? null : <RolledBackAlert stack={stack} />;
  }
  return <OwnerAlert stack={stack} />;
}

// RolledBackAlert is the alert of a pause after a roll back. The stack waits for review: the repository head is
// pending, "Review update" pulls it and opens the review, and "Turn on auto-deploy…" opens the same confirm as the
// switch on the Git tab, so a person decides to let the repository deploy again. The buttons need the permission to
// deploy; a user whose permissions are not known yet sees none.
function RolledBackAlert({ stack }: AutoDeployPausedAlertProps) {
  const t = useT();
  const git = stack.git;
  const needs = useNeeds("stacks.deploy", stack.envId);
  const review = usePullAndReview(stack);
  const environments = useQuery(environmentsQuery);
  const [confirming, setConfirming] = useState(false);
  if (!git) return null;

  const revision = git.autoDeployPausedRevision;
  const behind = git.status === "behind" && git.headSha !== "";
  const environment =
    environments.data?.items?.find((e) => e.id === stack.envId)?.name ?? stack.envId;
  const known = needs.allowed || needs.reason;
  return (
    <>
      <Alert role="status" aria-live="polite" data-slot="auto-deploy-rollback-alert">
        <Info aria-hidden="true" />
        <AlertTitle className="break-words">
          {revision
            ? t("stackGit.autoDeploy.rollbackAlert.title", { n: revision })
            : t("stackGit.autoDeploy.rollbackAlert.titleNoRevision")}
        </AlertTitle>
        <AlertDescription className="flex flex-col gap-3">
          <p className="mb-0! break-words">
            {behind
              ? t("stackGit.autoDeploy.rollbackAlert.body", {
                  name: stack.name,
                  sha: shortSha(git.headSha),
                })
              : t("stackGit.autoDeploy.rollbackAlert.bodyNoHead", { name: stack.name })}
          </p>
          {known ? (
            <div className="flex flex-wrap items-start gap-2">
              {behind ? (
                <NeedsTooltip reason={needs.allowed ? undefined : needs.reason}>
                  <Button
                    type="button"
                    variant="outline"
                    className="max-md:h-11"
                    aria-disabled={review.pending}
                    onClick={needs.allowed ? () => void review.pull() : undefined}
                  >
                    {review.pending ? t("stackGit.pull.pulling") : t("stackGit.pull.reviewUpdate")}
                  </Button>
                </NeedsTooltip>
              ) : null}
              <NeedsTooltip reason={needs.allowed ? undefined : needs.reason}>
                <Button
                  type="button"
                  variant="outline"
                  className="max-md:h-11"
                  aria-disabled={!needs.allowed}
                  onClick={needs.allowed ? () => setConfirming(true) : undefined}
                >
                  {t("stackGit.autoDeploy.rollbackAlert.turnOn")}
                </Button>
              </NeedsTooltip>
            </div>
          ) : null}
        </AlertDescription>
      </Alert>
      <AutoDeployConfirmDialog
        envId={stack.envId}
        stackId={stack.id}
        stackName={stack.name}
        gitRef={git.ref}
        folder={git.folder}
        environmentName={environment}
        open={confirming}
        onOpenChange={setConfirming}
      />
    </>
  );
}

// OwnerAlert is the alert of a pause because the owner no longer holds the permission to deploy on the environment: it
// names them and says to transfer it or turn it off. The owner's name is read from the git route, which the stack's own
// answer does not carry.
function OwnerAlert({ stack }: AutoDeployPausedAlertProps) {
  const t = useT();
  const git = stack.git;
  const show =
    git?.autoDeploy === true &&
    git.autoDeployPaused &&
    git.autoDeployPausedReason === "owner" &&
    git.status !== "error";
  const summary = useQuery({ ...stackGitQuery(stack.envId, stack.id), enabled: show });
  const environments = useQuery({ ...environmentsQuery, enabled: show });
  if (!show) return null;

  const user = summary.data?.autoDeployOwner?.name;
  const environment =
    environments.data?.items?.find((e) => e.id === stack.envId)?.name ?? stack.envId;
  return (
    <Alert variant="warning" role="status" aria-live="polite" data-slot="auto-deploy-paused-alert">
      <CirclePause aria-hidden="true" />
      <AlertTitle>{t("stackGit.autoDeploy.pausedAlert.title")}</AlertTitle>
      <AlertDescription className="flex flex-col gap-3">
        <p className="mb-0! break-words">
          {user
            ? t("stackGit.autoDeploy.pausedAlert.body", { user, environment })
            : t("stackGit.autoDeploy.pausedAlert.bodyNoUser", { environment })}
        </p>
        <div className="flex flex-wrap items-start gap-2">
          <Button asChild variant="outline" className="max-md:h-11">
            <Link
              to="/env/$envId/stacks/$stackId/git"
              params={{ envId: stack.envId, stackId: stack.id }}
            >
              {t("stackGit.autoDeploy.pausedAlert.openSettings")}
            </Link>
          </Button>
        </div>
      </AlertDescription>
    </Alert>
  );
}

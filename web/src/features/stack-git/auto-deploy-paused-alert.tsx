// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CirclePause } from "lucide-react";

import { environmentsQuery } from "@/api/environments";
import { stackGitQuery } from "@/api/stack-git";
import type { StackDetail } from "@/api/stacks";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { useT } from "@/i18n/use-t";

interface AutoDeployPausedAlertProps {
  stack: StackDetail;
}

// AutoDeployPausedAlert is the page's notice that a git stack's automatic deploys are paused (UI-SPEC "Stack page (git
// stack)", D-18). This is the owner variant: the person they run for no longer holds the permission to deploy on the
// environment, so it names them and says to transfer it or turn it off. It is state and not a notification, so it has no
// dismiss, and a stack shows at most one of the sync error and this alert (the error wins). A pause after a roll back
// has its own words and is shown by the plan that adds the roll back note; this draws nothing for it. The owner's name
// is read from the git route, which the stack's own answer does not carry. Every name is drawn as text.
export function AutoDeployPausedAlert({ stack }: AutoDeployPausedAlertProps) {
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

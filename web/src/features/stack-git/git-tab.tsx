// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Navigate } from "@tanstack/react-router";

import { stackGitQuery } from "@/api/stack-git";
import { stackQuery } from "@/api/stacks";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { AutoDeployCard } from "@/features/stack-git/auto-deploy-card";
import { DetachCard } from "@/features/stack-git/detach-card";
import { SourceCard } from "@/features/stack-git/source-card";
import { UpdatesCard, UpdatesSkeleton } from "@/features/stack-git/updates-card";
import { WebhookCard } from "@/features/stack-git/webhook-card";
import { useT } from "@/i18n/use-t";

interface GitTabProps {
  envId: string;
  stackId: string;
}

// GitTab is the Git tab of a git stack (UI-SPEC "Git tab"): one column of cards, each saved on its own, in the order
// Source, Updates, Webhook, Deploying and Advanced (Detach, shown to an Admin of the environment, closed), 32px apart and
// no wider than 1152px. A stack that is not from a repository has no Git tab: its address leads to the Overview.
export function GitTab({ envId, stackId }: GitTabProps) {
  const t = useT();
  const stack = useQuery(stackQuery(envId, stackId));
  const git = useQuery(stackGitQuery(envId, stackId));

  if (stack.data && stack.data.source !== "git") {
    return <Navigate to="/env/$envId/stacks/$stackId" params={{ envId, stackId }} replace />;
  }

  if (git.isError && !git.data) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("stackGit.tab.error.title")}</span>
          <span>{t("stackGit.tab.error.body")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={git.isFetching}
            onClick={() => void git.refetch()}
          >
            {t("stackGit.tab.error.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  return (
    <div className="flex max-w-6xl flex-col gap-8">
      {git.data ? (
        <>
          <SourceCard
            envId={envId}
            stackId={stackId}
            stackName={stack.data?.name ?? ""}
            git={git.data}
          />
          <UpdatesCard envId={envId} stackId={stackId} git={git.data} />
          <WebhookCard
            envId={envId}
            stackId={stackId}
            stackName={stack.data?.name ?? ""}
            git={git.data}
          />
          <AutoDeployCard
            envId={envId}
            stackId={stackId}
            stackName={stack.data?.name ?? ""}
            git={git.data}
          />
          <DetachCard
            envId={envId}
            stackId={stackId}
            stackName={stack.data?.name ?? ""}
            repoName={git.data.repoName}
            revision={stack.data?.deployedRevision || undefined}
            commit={git.data.deployedSha}
            hasDraft={stack.data?.hasDraft}
          />
        </>
      ) : (
        <UpdatesSkeleton />
      )}
    </div>
  );
}

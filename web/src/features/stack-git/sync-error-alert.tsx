// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Link } from "@tanstack/react-router";
import { RefreshCw, TriangleAlert } from "lucide-react";

import { useCheckStackGit } from "@/api/stack-git";
import type { StackDetail } from "@/api/stacks";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { shortSha } from "@/features/stack-git/sha-link";
import { reasonOfGit } from "@/features/stack-git/sync-status";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

interface SyncErrorAlertProps {
  stack: StackDetail;
}

// SyncErrorAlert is the page's notice that the stack cannot sync with its repository (UI-SPEC "Stack page (git stack)",
// GIT-09): which stack and which repository, why in the words of the reason, and that the last good version keeps
// running. It is a polite status and not an alert: a check can raise or clear it while the person reads, and focus never
// moves because of it. It has no dismiss, since it is state and not a notification, and it shows while the status is
// error and for nothing else: a stack that is behind, in sync, not checked, or behind because its source changed has
// none. Check now asks for another check; Open git settings leads to the Git tab. Every word of the repository's and
// the server's is drawn as text.
export function SyncErrorAlert({ stack }: SyncErrorAlertProps) {
  const t = useT();
  const git = stack.git;
  const check = useCheckStackGit(stack.envId, stack.id, {
    onError: () => toastError(t("stackGit.check.failed")),
  });
  if (git?.status !== "error") return null;

  const reason = reasonOfGit(t, git) ?? git.reasonDetail ?? t("stackGit.badge.errorTip");
  const checking = git.checking || check.running;
  return (
    <Alert variant="warning" role="status" aria-live="polite" data-slot="sync-error-alert">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle className="break-words">
        {git.repoName
          ? t("stackGit.alert.title", { name: stack.name, repo: git.repoName })
          : t("stackGit.alert.titleNoRepo", { name: stack.name })}
      </AlertTitle>
      <AlertDescription className="flex flex-col gap-3">
        <p className="mb-0! break-words">
          {git.deployedSha
            ? t("stackGit.alert.body", { reason, sha: shortSha(git.deployedSha) })
            : reason}
        </p>
        <div className="flex flex-wrap items-start gap-2">
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            disabled={checking}
            onClick={check.check}
          >
            <RefreshCw
              aria-hidden="true"
              className={checking ? "animate-spin motion-reduce:animate-none" : undefined}
            />
            {checking ? t("stackGit.alert.checking") : t("stackGit.alert.checkNow")}
          </Button>
          <Button asChild variant="outline" className="max-md:h-11">
            <Link
              to="/env/$envId/stacks/$stackId/git"
              params={{ envId: stack.envId, stackId: stack.id }}
            >
              {t("stackGit.alert.openSettings")}
            </Link>
          </Button>
        </div>
      </AlertDescription>
    </Alert>
  );
}

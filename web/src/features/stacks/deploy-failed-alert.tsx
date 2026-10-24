// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";
import { TriangleAlert } from "lucide-react";

import type { StackDetail } from "@/api/stacks";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { RollbackAction } from "@/features/stack-deploy/deploy-result";
import { keyExists, useT } from "@/i18n/use-t";

interface DeployFailedAlertProps {
  stack: StackDetail;
}

type Translate = (key: string, options?: Record<string, unknown>) => string;

// The operations a roll back can undo the effects of.
const DEPLOYING = new Set(["deploy", "update", "rollback"]);

// reasonOf is the sentence for how an operation failed, from its code. The server's code is a stable
// word; this words it, and a code it has no words for gets the plain one.
function reasonOf(t: Translate, code: string): string {
  const key = `stackDeploy.lastFailed.reason.${code}`;
  return t(keyExists(key) ? key : "stackDeploy.lastFailed.reason.default");
}

// DeployFailedAlert is the warning on a stack's page after its last deploy stopped (UI-SPEC "Stack
// page", D-14): it says where Bayplan stopped and why, and offers the console and the one-click way back
// to the version that ran well. It stays until a later deploy succeeds, because it is drawn from the
// stack's newest operation. A deploy that was cancelled or that a restart interrupted is told as that.
// A service name and a code are text. Nothing here rolls back by itself.
export function DeployFailedAlert({ stack }: DeployFailedAlertProps) {
  const t = useT();
  const op = stack.lastOperation;
  if (stack.state !== "deploy_failed" || !op || !DEPLOYING.has(op.kind)) return null;

  const rollback = op.kind === "rollback";
  const cancelled = op.state === "cancelled";
  const title = t(
    rollback
      ? cancelled
        ? "stackDeploy.lastFailed.titleRollbackCancelled"
        : "stackDeploy.lastFailed.titleRollback"
      : cancelled
        ? "stackDeploy.lastFailed.titleCancelled"
        : "stackDeploy.lastFailed.title",
  );
  let body: string;
  switch (true) {
    case op.state === "interrupted":
      body = t(
        rollback
          ? "stackDeploy.lastFailed.interruptedRollback"
          : "stackDeploy.lastFailed.interrupted",
      );
      break;
    case cancelled:
      body = op.failedService
        ? t("stackDeploy.lastFailed.stoppedAtCancelled", { service: op.failedService })
        : t("stackDeploy.lastFailed.stoppedCancelled");
      break;
    case Boolean(op.failedService):
      body = t("stackDeploy.lastFailed.stoppedAt", {
        service: op.failedService,
        reason: reasonOf(t, op.code),
      });
      break;
    default:
      body = t("stackDeploy.lastFailed.stopped", { reason: reasonOf(t, op.code) });
  }

  return (
    <Alert variant="warning" data-slot="deploy-failed">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>{title}</AlertTitle>
      <AlertDescription className="flex flex-col gap-3">
        <p className="break-words">{body}</p>
        <div className="flex flex-wrap items-start gap-2">
          {op.jobId ? (
            <Button asChild variant="outline" className="max-md:h-11">
              <Link
                to="/env/$envId/stacks/$stackId/deploys/$jobId"
                params={{ envId: stack.envId, stackId: stack.id, jobId: op.jobId }}
              >
                {t("stackDeploy.lastFailed.openConsole")}
              </Link>
            </Button>
          ) : null}
          {/* The caption names the revision the one-click roll back restores, when it was deployed and by whom. */}
          <RollbackAction
            envId={stack.envId}
            stackId={stack.id}
            retry={rollback}
            variant="outline"
          />
        </div>
      </AlertDescription>
    </Alert>
  );
}

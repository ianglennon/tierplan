// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { Lock } from "lucide-react";

import type { StackLock } from "@/api/stacks";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { useT } from "@/i18n/use-t";

interface OperationLockAlertProps {
  lock: StackLock;
  envId: string;
  envName: string;
}

// OperationLockAlert is the warning that another stack operation holds the environment (UI-SPEC
// "Operation lock"): only one runs at a time, and a second is refused and not queued. It names the
// stack it is running on and when it started, and Open it goes to the console of that operation, or
// to the stack when the job is not known yet. The stack's name is text.
export function OperationLockAlert({ lock, envId, envName }: OperationLockAlertProps) {
  const t = useT();
  const link = lock.jobId ? (
    <Link
      to="/env/$envId/stacks/$stackId/deploys/$jobId"
      params={{ envId, stackId: lock.stackId, jobId: lock.jobId }}
      className="rounded-sm text-accent-ink underline underline-offset-3"
    >
      {t("stackConverge.lock.openIt")}
    </Link>
  ) : (
    <Link
      to="/env/$envId/stacks/$stackId"
      params={{ envId, stackId: lock.stackId }}
      className="rounded-sm text-accent-ink underline underline-offset-3"
    >
      {t("stackConverge.lock.openIt")}
    </Link>
  );
  return (
    <Alert variant="warning" data-slot="operation-lock">
      <Lock aria-hidden="true" />
      <AlertDescription className="flex flex-wrap items-center gap-x-3 gap-y-1 text-status-warning-text">
        <span className="break-words">
          {t("stackConverge.lock.before", { env: envName, stack: lock.stackName })}{" "}
          <RelativeTime value={lock.startedAt} />
          {t("stackConverge.lock.after")}
        </span>
        {link}
      </AlertDescription>
    </Alert>
  );
}

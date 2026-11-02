// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { GitCompareArrows } from "lucide-react";

import type { StackDrift } from "@/api/stack-drift";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { DriftCause } from "@/features/stack-drift/drift-cause";
import { RestoreAction } from "@/features/stack-drift/restore-action";
import { useT } from "@/i18n/use-t";

interface DriftAlertProps {
  envId: string;
  stackId: string;
  name: string;
  // The newest drift result, absent until there is one.
  drift: Pick<StackDrift, "state" | "revision" | "count" | "cause" | "causes"> | undefined;
}

// DriftAlert is the Overview's notice that what runs no longer matches the deployed revision (UI-SPEC "Drift"). It is
// a polite status and not an alert: a check can raise or clear it while the person reads, and focus never moves
// because of it. It draws nothing for any other state: a stopped stack, a stack that was never deployed and a check that
// could not be made do not drift. It says why when the audit log knows, and that the change came from outside TierPlan
// when it does not (D-24). The name is text.
export function DriftAlert({ envId, stackId, name, drift }: DriftAlertProps) {
  const t = useT();
  if (drift?.state !== "drifted") return null;
  return (
    <Alert variant="warning" role="status" aria-live="polite" data-slot="drift-alert">
      <GitCompareArrows aria-hidden="true" />
      <AlertTitle className="break-words">
        {t("stackDrift.alert.title", { name, revision: drift.revision })}
      </AlertTitle>
      <AlertDescription className="flex flex-col gap-3">
        <div className="flex flex-col gap-1">
          <p className="mb-0!">{t("stackDrift.alert.body", { count: drift.count })}</p>
          <p className="mb-0!" data-slot="drift-cause">
            <DriftCause cause={drift.cause} count={drift.causes} stackName={name} />
          </p>
        </div>
        <div className="flex flex-wrap items-start gap-2">
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/stacks/$stackId/drift" params={{ envId, stackId }}>
              {t("stackDrift.alert.view")}
            </Link>
          </Button>
          <RestoreAction envId={envId} stackId={stackId} drifted variant="outline" ellipsis />
        </div>
      </AlertDescription>
    </Alert>
  );
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";
import { GitCompareArrows } from "lucide-react";

import type { StackDrift } from "@/api/stack-drift";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { useT } from "@/i18n/use-t";

interface DriftAlertProps {
  envId: string;
  stackId: string;
  name: string;
  // The newest drift result, absent until there is one.
  drift: Pick<StackDrift, "state" | "revision" | "count"> | undefined;
}

// DriftAlert is the Overview's notice that what runs no longer matches the deployed revision (UI-SPEC "Drift"). It is
// a polite status and not an alert: a check can raise or clear it while the person reads, and focus never moves
// because of it. It draws nothing for any other state: a stopped stack, a stack that was never deployed and a check that
// could not be made do not drift. The name is text.
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
        <p>{t("stackDrift.alert.body", { count: drift.count })}</p>
        <div className="flex flex-wrap items-start gap-2">
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/stacks/$stackId/drift" params={{ envId, stackId }}>
              {t("stackDrift.alert.view")}
            </Link>
          </Button>
        </div>
      </AlertDescription>
    </Alert>
  );
}

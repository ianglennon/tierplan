// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { CirclePause } from "lucide-react";

import type { StackDetail } from "@/api/stacks";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { useT } from "@/i18n/use-t";

interface StoppedAlertProps {
  stack: StackDetail;
  // What Start stack does. The button is drawn only when something is wired to it: the plan that
  // adds Stop and Start passes it, and until then the alert says the stack is stopped and offers no
  // button that would do nothing.
  onStart?: () => void;
}

// StoppedAlert says the stack was stopped by a person and what stopping kept (UI-SPEC "Stack
// page"). It renders only for a stack that is stopped. The name is text.
export function StoppedAlert({ stack, onStart }: StoppedAlertProps) {
  const t = useT();
  if (!stack.stopped) return null;
  return (
    <Alert data-slot="stopped-alert">
      <CirclePause aria-hidden="true" />
      <AlertTitle className="break-words">
        {t("stackOverview.stopped.title", { name: stack.name })}
      </AlertTitle>
      <AlertDescription className="flex flex-col gap-3">
        <p>{t("stackOverview.stopped.body")}</p>
        {onStart ? (
          <div className="flex flex-wrap items-start gap-2">
            <Button type="button" variant="outline" className="max-md:h-11" onClick={onStart}>
              {t("stackOverview.stopped.start")}
            </Button>
          </div>
        ) : null}
      </AlertDescription>
    </Alert>
  );
}

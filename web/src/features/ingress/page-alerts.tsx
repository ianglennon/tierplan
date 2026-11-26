// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { useRef } from "react";

import { IngressActionError, ingressStatusQuery, useResumeIngress } from "@/api/ingress";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// IngressPageAlerts draws the states of the whole environment once, above the tab body and never inside a tab
// (UI-SPEC S1): that TierPlan cannot reach the environment, and that ingress is paused after a restore. Tabs still
// draw from their last answers under them. A status that cannot be read draws nothing: each tab reports its own
// failed read.
export function IngressPageAlerts({ envId }: { envId: string }) {
  const t = useT();
  const status = useQuery(ingressStatusQuery(envId)).data;
  const manage = useNeeds("ingress.manage", envId);
  const resume = useResumeIngress(envId);
  // A second press can land before the pending state has rendered.
  const inFlight = useRef(false);

  if (!status) return null;
  const unreachable = !status.objectsKnown || status.lastEnsure.class === "unreachable";
  if (!unreachable && !status.paused) return null;

  function press() {
    if (inFlight.current) return;
    inFlight.current = true;
    resume.mutate(undefined, {
      onSettled: () => {
        inFlight.current = false;
      },
    });
  }

  const refusal = resume.error instanceof IngressActionError ? resume.error : undefined;
  const refused = resume.isError
    ? (refusal?.detail ?? t("ingressPage.alerts.resumeFailed"))
    : undefined;

  return (
    <div className="flex flex-col gap-3">
      {unreachable ? (
        <Alert>
          <AlertDescription>{t("ingressPage.alerts.unreachable")}</AlertDescription>
        </Alert>
      ) : null}
      {status.paused ? (
        <Alert variant="warning">
          <AlertDescription className="flex flex-col gap-2">
            <span>{t("ingressPage.alerts.paused")}</span>
            <NeedsTooltip reason={manage.allowed ? undefined : manage.reason}>
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="self-start max-md:h-11"
                disabled={resume.isPending}
                onClick={press}
              >
                {t("ingressPage.alerts.resume")}
              </Button>
            </NeedsTooltip>
            {refused ? (
              <Alert variant="destructive">
                <AlertDescription className="break-words">{refused}</AlertDescription>
              </Alert>
            ) : null}
          </AlertDescription>
        </Alert>
      ) : null}
    </div>
  );
}

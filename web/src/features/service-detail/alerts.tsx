// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { TriangleAlert } from "lucide-react";
import type { ReactNode } from "react";

import { type RolloutState, type ServiceDetail, serviceRolloutQuery } from "@/api/services";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { ManagedByStackNotice } from "@/features/services/managed-by-stack-notice";
import { useT } from "@/i18n/use-t";

// The rollout states whose alert or progress region stays on the page. The replicas-short alert
// gives way to them: the update explains why tasks are missing (UI-SPEC "Service detail").
const ROLLOUT_ALERT_STATES: readonly RolloutState[] = [
  "updating",
  "rollback_started",
  "paused",
  "rollback_paused",
  "rollback_completed",
];

interface ServiceAlertsProps {
  envId: string;
  detail: ServiceDetail;
  // The restart-loop alert, which a later plan (06-34) supplies. When it is given, the
  // replicas-short alert is not shown.
  restartLoop?: ReactNode;
}

// ServiceAlerts are the alerts under a service's header, after the update-state region
// (RolloutProgress) that sits above them: the restart-loop alert, the replicas-short alert, the
// stack-owned notice and the agent-service warning, in that order. The replicas-short alert is
// suppressed while an update-state alert or the restart-loop alert is shown, so a user sees at
// most the update-state and restart-loop alerts together (D-12). Docker's task error is text.
export function ServiceAlerts({ envId, detail, restartLoop }: ServiceAlertsProps) {
  const t = useT();
  const query = useQuery(serviceRolloutQuery(envId, detail.id));
  const rollout = query.data;
  // Until the rollout has been read (or has failed to be) the alert would flash up for an update
  // that is about to explain it.
  const rolloutRead = rollout !== undefined || query.isError;

  const rolloutShown = rollout !== undefined && ROLLOUT_ALERT_STATES.includes(rollout.state);
  const loopShown = restartLoop !== undefined && restartLoop !== null && restartLoop !== false;
  const hasReplicaCount = detail.mode === "replicated" || detail.mode === "global";
  const short = hasReplicaCount && detail.desired > detail.running;

  return (
    <>
      {loopShown ? restartLoop : null}
      {short && rolloutRead && !rolloutShown && !loopShown ? (
        <Alert variant="warning" role="group">
          <TriangleAlert aria-hidden="true" />
          <AlertTitle>
            {t("servicedetail.alerts.replicasShort.title", {
              count: detail.desired - detail.running,
              desired: detail.desired,
            })}
          </AlertTitle>
          <AlertDescription className="break-words">
            {rollout?.latestTaskError
              ? t("servicedetail.alerts.replicasShort.error", { error: rollout.latestTaskError })
              : t("servicedetail.alerts.replicasShort.starting")}
          </AlertDescription>
        </Alert>
      ) : null}
      {detail.stack ? <ManagedByStackNotice stack={detail.stack} /> : null}
      {detail.agent ? (
        <Alert variant="warning">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="text-foreground">
            {t("servicedetail.agentService")}
          </AlertDescription>
        </Alert>
      ) : null}
    </>
  );
}

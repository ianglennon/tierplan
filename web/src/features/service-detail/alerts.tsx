// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CircleX, TriangleAlert } from "lucide-react";

import { type RolloutState, type ServiceDetail, serviceRolloutQuery } from "@/api/services";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
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
}

// RestartLoopAlert says the service's tasks keep failing (RES-13, D-14): the count and the
// window, Docker's latest error as wrapping text, and a link to the Tasks tab with only the
// failed tasks. The icon is the offline red, the alert the warning treatment.
function RestartLoopAlert({ envId, detail }: { envId: string; detail: ServiceDetail }) {
  const t = useT();
  const loop = detail.restartLoop;
  if (!loop) return null;
  const window = t("restartLoop.minutes", { count: loop.windowMinutes });
  const title = t("restartLoop.alert.title", { name: detail.name });
  return (
    <Alert variant="warning" role="group" aria-label={title}>
      <CircleX aria-hidden="true" className="text-status-offline-text" />
      <AlertTitle className="break-words">{title}</AlertTitle>
      <AlertDescription className="break-words">
        {loop.latestError
          ? t("restartLoop.alert.latest", { count: loop.count, window, error: loop.latestError })
          : t("restartLoop.alert.body", { count: loop.count, window })}
      </AlertDescription>
      <div className="col-start-2 mt-2 flex flex-wrap gap-2">
        <Button asChild variant="outline" size="sm" className="max-md:h-11">
          <Link
            to="/env/$envId/services/$serviceId/tasks"
            params={{ envId, serviceId: detail.id }}
            search={{ failed: 1 }}
          >
            {t("restartLoop.alert.viewFailed")}
          </Link>
        </Button>
      </div>
    </Alert>
  );
}

// ServiceAlerts are the alerts under a service's header, after the update-state region
// (RolloutProgress) that sits above them: the restart-loop alert, the replicas-short alert, the
// stack-owned notice and the agent-service warning, in that order. The replicas-short alert is
// suppressed while an update-state alert or the restart-loop alert is shown, so a user sees at
// most the update-state and restart-loop alerts together (D-12). Docker's task error is text.
export function ServiceAlerts({ envId, detail }: ServiceAlertsProps) {
  const t = useT();
  const query = useQuery(serviceRolloutQuery(envId, detail.id));
  const rollout = query.data;
  // Until the rollout has been read (or has failed to be) the alert would flash up for an update
  // that is about to explain it.
  const rolloutRead = rollout !== undefined || query.isError;

  const rolloutShown = rollout !== undefined && ROLLOUT_ALERT_STATES.includes(rollout.state);
  const loopShown = detail.restartLoop !== undefined;
  const hasReplicaCount = detail.mode === "replicated" || detail.mode === "global";
  const short = hasReplicaCount && detail.desired > detail.running;

  return (
    <>
      {loopShown ? <RestartLoopAlert envId={envId} detail={detail} /> : null}
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

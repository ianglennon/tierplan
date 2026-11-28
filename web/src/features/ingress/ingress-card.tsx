// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import { ingressStatusQuery } from "@/api/ingress";
import {
  useDisableIngress,
  useEnableIngress,
  useIngressDisablePreview,
  useIngressEnablePreview,
} from "@/api/ingress-switches";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { SettingsCard } from "@/features/environments/settings-card";
import { REDEPLOY_SHOWN, ReviewDialog, type ReviewKind } from "@/features/ingress/review-dialog";
import { usingRoute } from "@/features/ingress/self-route-card";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// IngressCard is the Settings tab's first card (UI-SPEC S6, D-05): whether ingress is on for the environment and how
// many of its proxy nodes are ready, with the button that switches it. A switch is never made at once (D-07): the
// button opens the review dialog, which shows the server's preview and waits for the confirm. A reader sees the status
// and the button disabled with the reason, and never starts a job (A-02).
export function IngressCard({ envId }: { envId: string }) {
  const t = useT();
  const status = useQuery(ingressStatusQuery(envId));
  const needs = useNeeds("ingress.manage", envId);
  const [reviewing, setReviewing] = useState<ReviewKind | null>(null);
  // The dialog keeps the kind it was opened for until it has gone, so its words do not change while it fades out.
  const [shown, setShown] = useState<ReviewKind>("enable");
  // True while a job started from this card, in this session, runs. A job started elsewhere shows as the server's
  // operation_locked sentence in the dialog (D-05), since this page has no way to read it before then.
  const [running, setRunning] = useState(false);
  const enablePreview = useIngressEnablePreview(envId);
  const enable = useEnableIngress(envId);
  const disablePreview = useIngressDisablePreview(envId);
  const disable = useDisableIngress(envId);

  function review(kind: ReviewKind) {
    if (!needs.allowed) return;
    setShown(kind);
    setReviewing(kind);
  }

  const title = t("ingressPage.settings.ingress.title");
  if (status.isPending) {
    return (
      <SettingsCard title={title}>
        <Skeleton className="h-16" />
      </SettingsCard>
    );
  }
  if (status.isError) {
    return (
      <SettingsCard title={title}>
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("ingressPage.settings.ingress.error")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={status.isFetching}
              onClick={() => void status.refetch()}
            >
              {t("ingressPage.settings.ingress.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      </SettingsCard>
    );
  }

  const on = status.data.enabled;
  const enableFailed = !on && status.data.enableFailed;
  const incomplete = !on && !enableFailed && status.data.disablingIncomplete;
  // A stuck switch offers both directions, as the server's sentence says: carry on, or take back what was made.
  const stuck = enableFailed || incomplete;
  const redeploy = on ? (status.data.redeploy ?? []) : [];
  const wipes = status.data.pendingWipes;
  // TierPlan's own route, when it is on here: switching ingress off releases it first (UI-SPEC S4), and a person using
  // TierPlan through its host is warned as the route card warns (S1c). The server's empty direct address is this origin.
  const self = status.data.selfRoute?.on ? status.data.selfRoute : undefined;
  const direct = self?.directAddress ? self.directAddress : window.location.origin;
  const caution =
    shown === "disable" && self && usingRoute(self.host)
      ? t("ingressPage.settings.selfRoute.offUsingRoute", { direct })
      : undefined;
  const statusKey = on
    ? "ingressPage.settings.ingress.on"
    : enableFailed
      ? "ingressPage.settings.ingress.enableFailed"
      : incomplete
        ? "ingressPage.settings.ingress.disablingIncomplete"
        : "ingressPage.settings.ingress.off";

  function button(direction: ReviewKind) {
    return (
      <NeedsTooltip reason={needs.reason}>
        <Button
          type="button"
          variant="outline"
          className="max-md:h-11"
          disabled={running || (!needs.allowed && !needs.reason)}
          onClick={() => review(direction)}
        >
          {t(
            direction === "disable"
              ? "ingressPage.settings.ingress.turnOff"
              : "ingressPage.settings.ingress.turnOn",
          )}
        </Button>
      </NeedsTooltip>
    );
  }

  return (
    <SettingsCard title={title}>
      <div className="flex flex-col gap-1">
        <StatusDot
          state={on ? "online" : stuck ? "degraded" : "pending"}
          label={t(statusKey)}
          className="text-sm"
        />
        <p className="text-sm text-muted-foreground">
          {on
            ? t("ingressPage.settings.ingress.ready", {
                ready: status.data.nodes.ready,
                expected: status.data.nodes.expected,
              })
            : stuck
              ? null
              : t("ingressPage.settings.ingress.offBody")}
        </p>
      </div>
      {enableFailed ? (
        <Alert variant="warning">
          <AlertDescription>{t("ingressPage.settings.ingress.enableFailedBody")}</AlertDescription>
        </Alert>
      ) : null}
      {incomplete ? (
        <Alert variant="warning">
          <AlertDescription>
            {t("ingressPage.settings.ingress.disablingIncompleteBody")}
          </AlertDescription>
        </Alert>
      ) : null}
      {wipes > 0 ? (
        <Alert>
          <AlertDescription>
            {t("ingressPage.settings.ingress.pendingWipes", { count: wipes })}
          </AlertDescription>
        </Alert>
      ) : null}
      {redeploy.length > 0 ? (
        <Alert>
          <AlertDescription className="flex flex-col gap-1">
            <span>{t("ingressPage.settings.ingress.redeploy", { count: redeploy.length })}</span>
            <ul className="flex flex-col gap-0.5">
              {redeploy.slice(0, REDEPLOY_SHOWN).map((stack) => (
                <li key={stack.stackId} className="font-mono text-xs break-all">
                  {stack.stackName}
                </li>
              ))}
            </ul>
            {redeploy.length > REDEPLOY_SHOWN ? (
              <span className="text-xs text-muted-foreground">
                {t("ingressPage.review.redeploy.more", { n: redeploy.length - REDEPLOY_SHOWN })}
              </span>
            ) : null}
          </AlertDescription>
        </Alert>
      ) : null}
      {running ? (
        <div className="flex flex-wrap items-center gap-3">
          <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
            <Spinner aria-hidden="true" />
            {t(`ingressPage.review.${shown}.progress`)}
          </p>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            onClick={() => setReviewing(shown)}
          >
            {t("ingressPage.settings.ingress.showProgress")}
          </Button>
        </div>
      ) : null}
      <div className="flex flex-wrap gap-2">
        {stuck ? (
          <>
            {button("enable")}
            {button("disable")}
          </>
        ) : (
          button(on ? "disable" : "enable")
        )}
      </div>
      <ReviewDialog
        envId={envId}
        kind={shown}
        onRunning={setRunning}
        open={reviewing !== null}
        onOpenChange={(open) => {
          if (!open) setReviewing(null);
        }}
        preview={shown === "enable" ? enablePreview : disablePreview}
        start={shown === "enable" ? enable : disable}
        caution={caution}
      />
    </SettingsCard>
  );
}

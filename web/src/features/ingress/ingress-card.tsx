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
import { SettingsCard } from "@/features/environments/settings-card";
import { ReviewDialog, type ReviewKind } from "@/features/ingress/review-dialog";
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
  const kind: ReviewKind = on ? "disable" : "enable";
  return (
    <SettingsCard title={title}>
      <div className="flex flex-col gap-1">
        <StatusDot
          state={on ? "online" : "pending"}
          label={t(on ? "ingressPage.settings.ingress.on" : "ingressPage.settings.ingress.off")}
          className="text-sm"
        />
        <p className="text-sm text-muted-foreground">
          {on
            ? t("ingressPage.settings.ingress.ready", {
                ready: status.data.nodes.ready,
                expected: status.data.nodes.expected,
              })
            : t("ingressPage.settings.ingress.offBody")}
        </p>
      </div>
      <div>
        <NeedsTooltip reason={needs.reason}>
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            disabled={!needs.allowed && !needs.reason}
            onClick={() => review(kind)}
          >
            {t(on ? "ingressPage.settings.ingress.turnOff" : "ingressPage.settings.ingress.turnOn")}
          </Button>
        </NeedsTooltip>
      </div>
      <ReviewDialog
        envId={envId}
        kind={shown}
        open={reviewing !== null}
        onOpenChange={(open) => {
          if (!open) setReviewing(null);
        }}
        preview={shown === "enable" ? enablePreview : disablePreview}
        start={shown === "enable" ? enable : disable}
      />
    </SettingsCard>
  );
}

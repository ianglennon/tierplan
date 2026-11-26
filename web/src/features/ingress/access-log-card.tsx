// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useId } from "react";

import { IngressActionError, ingressStatusQuery } from "@/api/ingress";
import { useSetAccessLogs } from "@/api/ingress-switches";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { SettingsCard } from "@/features/environments/settings-card";
import { PROXY_CONTAINER, PROXY_SERVICE } from "@/features/ingress/routes-tab";
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

// AccessLogCard is the Settings tab's third card (UI-SPEC S6 card 3, D-05, U-10): whether the proxies write an access
// log. The API has no preview for this switch, so it is the one control that commits at once: the switch moves, the
// request goes, a toast says so, and a failure puts the switch back and says why in the server's words. While the
// request is in flight the switch is off and a Spinner stands beside it. A reader sees the value and the switch
// disabled with the reason.
export function AccessLogCard({ envId }: { envId: string }) {
  const t = useT();
  const id = useId();
  const status = useQuery(ingressStatusQuery(envId));
  const needs = useNeeds("ingress.manage", envId);
  const swarm = useEnvIsSwarm(envId);
  // The proxy's logs are a link to the log view the page already has, and only for someone who may read it there.
  const logsRead = useNeeds(swarm ? "services.read" : "containers.read", envId);
  const set = useSetAccessLogs(envId);

  const title = t("ingressPage.settings.accessLog.title");
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

  // While the request is in flight the switch shows where it is going, and once it has answered it shows what the
  // server stored: a refusal leaves the status as it was, so the switch is back where it started.
  const on = set.isPending ? set.variables === true : status.data.accessLogs;
  const switchId = `${id}-switch`;

  function toggle(next: boolean) {
    if (!needs.allowed || set.isPending) return;
    set.mutate(next, {
      onSuccess: () =>
        toastSuccess(
          t(
            next
              ? "ingressPage.settings.accessLog.toastOn"
              : "ingressPage.settings.accessLog.toastOff",
          ),
        ),
      onError: (error) =>
        toastError(
          (error instanceof IngressActionError ? error.detail : undefined) ??
            t("common.mutationFailed"),
        ),
    });
  }

  return (
    <SettingsCard title={title}>
      <StatusDot
        state={on ? "online" : "pending"}
        label={t(on ? "ingressPage.settings.accessLog.on" : "ingressPage.settings.accessLog.off")}
        className="text-sm"
      />
      <div className="flex flex-col gap-2">
        <div className="flex min-h-11 items-center gap-3 md:min-h-8">
          <NeedsTooltip reason={needs.reason}>
            <Switch
              id={switchId}
              checked={on}
              disabled={set.isPending}
              aria-describedby={`${switchId}-help`}
              onCheckedChange={toggle}
            />
          </NeedsTooltip>
          <label htmlFor={switchId} className="text-sm">
            {t("ingressPage.settings.accessLog.switch")}
          </label>
          {set.isPending ? <Spinner aria-hidden="true" /> : null}
        </div>
        <p id={`${switchId}-help`} className="text-xs text-muted-foreground">
          {t("ingressPage.settings.accessLog.helper")}
        </p>
        {logsRead.allowed ? (
          <Link
            {...(swarm
              ? {
                  to: "/env/$envId/services/$serviceId/logs" as const,
                  params: { envId, serviceId: PROXY_SERVICE },
                }
              : {
                  to: "/env/$envId/containers/$containerId/logs" as const,
                  params: { envId, containerId: PROXY_CONTAINER },
                })}
            className="w-fit rounded-sm text-sm text-accent-ink hover:underline max-md:py-3"
          >
            {t("ingressPage.routes.proxyLogs")}
          </Link>
        ) : null}
      </div>
    </SettingsCard>
  );
}

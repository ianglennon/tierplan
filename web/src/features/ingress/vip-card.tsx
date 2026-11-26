// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useId, useState } from "react";

import { ingressStatusQuery } from "@/api/ingress";
import { ingressVIPQuery } from "@/api/ingress-nodes";
import {
  useChangeVIP,
  useDisableVIP,
  useEnableVIP,
  useVIPDisablePreview,
  useVIPPreview,
  type VIPInput,
} from "@/api/ingress-switches";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { SettingsCard } from "@/features/environments/settings-card";
import { ReviewDialog, type ReviewKind } from "@/features/ingress/review-dialog";
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// The router ID is one byte of the VRRP header, and 0 is not a router.
const ROUTER_ID_MIN = 1;
const ROUTER_ID_MAX = 255;

// routerIdOf reads the router ID field: empty means TierPlan chooses, a whole number from 1 to 255 is an override,
// anything else is not usable yet (null).
function routerIdOf(text: string): number | undefined | null {
  const trimmed = text.trim();
  if (trimmed === "") return undefined;
  if (!/^\d+$/.test(trimmed)) return null;
  const n = Number(trimmed);
  return n >= ROUTER_ID_MIN && n <= ROUTER_ID_MAX ? n : null;
}

// VIPCard is the Settings tab's second card (UI-SPEC S6 card 2, D-05): the swarm's virtual IP, one address that always
// points at a working proxy. It is not drawn on a standalone host, where there is no second node to move to (Pitfall
// 14). Like every switch it never writes at once (D-07): the button opens the review dialog, whose first state is the
// form, then the server's preview with the fixed privileged-services warning first, then the job. A reader sees the
// status and the button disabled with the reason.
export function VIPCard({ envId }: { envId: string }) {
  const t = useT();
  const id = useId();
  const swarm = useEnvIsSwarm(envId);
  const status = useQuery(ingressStatusQuery(envId));
  const vipQuery = useQuery(ingressVIPQuery(envId, swarm));
  const needs = useNeeds("ingress.manage", envId);
  const [reviewing, setReviewing] = useState<ReviewKind | null>(null);
  // The dialog keeps the kind it was opened for until it has gone, so its words do not change while it fades out.
  const [shown, setShown] = useState<ReviewKind>("vipEnable");
  // The address a switch-off was opened for, so the dialog's sentence does not change when the job removes it.
  const [leaving, setLeaving] = useState("");
  const [address, setAddress] = useState("");
  const [routerId, setRouterId] = useState("");
  const [running, setRunning] = useState(false);

  const routerValue = routerIdOf(routerId);
  const input: VIPInput = {
    address,
    ...(typeof routerValue === "number" ? { routerId: routerValue } : {}),
  };
  const preview = useVIPPreview(envId, input);
  const enable = useEnableVIP(envId, input);
  const change = useChangeVIP(envId, input);
  const disablePreview = useVIPDisablePreview(envId);
  const disable = useDisableVIP(envId);

  if (!swarm) return null;

  const title = t("ingressPage.settings.vip.title");
  if (vipQuery.isPending || status.isPending) {
    return (
      <SettingsCard title={title}>
        <Skeleton className="h-16" />
      </SettingsCard>
    );
  }
  if (vipQuery.isError || status.isError) {
    return (
      <SettingsCard title={title}>
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("ingressPage.settings.vip.error")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={vipQuery.isFetching || status.isFetching}
              onClick={() => {
                void vipQuery.refetch();
                void status.refetch();
              }}
            >
              {t("ingressPage.settings.vip.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      </SettingsCard>
    );
  }

  const vip = vipQuery.data;
  const on = vip?.enabled === true;
  // A switch-off that failed part way leaves the row switched off with things still in the swarm: switching off again
  // finishes it, so the card offers that.
  const unfinished = vip != null && !vip.enabled;
  const ingressOn = status.data.enabled;
  // A permission reason comes first: a reader is told what they need, and an Admin is told what the page needs.
  const reason = !needs.allowed
    ? needs.reason
    : !ingressOn
      ? t("ingressPage.settings.vip.needsIngress")
      : undefined;

  function begin(kind: ReviewKind) {
    if (!needs.allowed) return;
    if (kind === "vipEnable" && !ingressOn) return;
    if (kind === "vipChange" && vip) {
      // A router ID TierPlan chose is left empty, so that it goes on choosing one; one an Admin chose is kept.
      setAddress(vip.address);
      setRouterId(vip.routerDerived ? "" : String(vip.routerId));
    } else {
      setAddress("");
      setRouterId("");
    }
    if (kind === "vipDisable") setLeaving(vip?.address ?? "");
    setShown(kind);
    setReviewing(kind);
  }

  const addressId = `${id}-address`;
  const routerIdId = `${id}-router-id`;
  const canCheck = address.trim() !== "" && routerValue !== null;
  const check = () => {
    if (canCheck && !preview.isPending) preview.mutate();
  };
  const enterChecks = (event: React.KeyboardEvent) => {
    if (event.key !== "Enter") return;
    event.preventDefault();
    check();
  };

  return (
    <SettingsCard title={title}>
      <div className="flex flex-col gap-1">
        <StatusDot
          state={on ? "online" : "pending"}
          label={t(on ? "ingressPage.settings.vip.on" : "ingressPage.settings.vip.off")}
          className="text-sm"
        />
        {on && vip ? (
          <dl className="grid gap-x-6 gap-y-1 text-sm md:grid-cols-[max-content_1fr]">
            <dt className="text-muted-foreground">{t("ingressPage.nodes.vip.address")}</dt>
            <dd className="font-mono break-all">{vip.address}</dd>
            <dt className="text-muted-foreground">{t("ingressPage.nodes.vip.routerId")}</dt>
            <dd className="font-mono">{vip.routerId}</dd>
          </dl>
        ) : (
          <p className="text-sm text-muted-foreground">{t("ingressPage.settings.vip.offBody")}</p>
        )}
      </div>
      {on ? (
        <Link
          to="/env/$envId/ingress/nodes"
          params={{ envId }}
          className="w-fit rounded-sm text-sm text-accent-ink hover:underline max-md:py-3"
        >
          {t("ingressPage.settings.vip.seeNodes")}
        </Link>
      ) : null}
      <div className="flex flex-wrap gap-2">
        {on ? (
          <>
            <NeedsTooltip reason={needs.reason}>
              <Button
                type="button"
                variant="outline"
                className="max-md:h-11"
                disabled={running}
                onClick={() => begin("vipChange")}
              >
                {t("ingressPage.settings.vip.change")}
              </Button>
            </NeedsTooltip>
            <NeedsTooltip reason={needs.reason}>
              <Button
                type="button"
                variant="outline"
                className="max-md:h-11"
                disabled={running}
                onClick={() => begin("vipDisable")}
              >
                {t("ingressPage.settings.vip.turnOff")}
              </Button>
            </NeedsTooltip>
          </>
        ) : (
          <>
            <NeedsTooltip reason={reason}>
              <Button
                type="button"
                variant="outline"
                className="max-md:h-11"
                disabled={running}
                onClick={() => begin("vipEnable")}
              >
                {t("ingressPage.settings.vip.turnOn")}
              </Button>
            </NeedsTooltip>
            {unfinished ? (
              <NeedsTooltip reason={needs.reason}>
                <Button
                  type="button"
                  variant="outline"
                  className="max-md:h-11"
                  disabled={running}
                  onClick={() => begin("vipDisable")}
                >
                  {t("ingressPage.settings.vip.turnOff")}
                </Button>
              </NeedsTooltip>
            ) : null}
          </>
        )}
      </div>
      <ReviewDialog
        envId={envId}
        kind={shown}
        open={reviewing !== null}
        onOpenChange={(next) => {
          if (!next) setReviewing(null);
        }}
        onRunning={setRunning}
        preview={shown === "vipDisable" ? disablePreview : preview}
        start={shown === "vipEnable" ? enable : shown === "vipChange" ? change : disable}
        bodyValues={{ address: leaving }}
        form={
          shown === "vipDisable"
            ? undefined
            : {
                canCheck,
                check,
                fields: (
                  <div className="flex flex-col gap-4">
                    <div className="flex flex-col gap-1.5">
                      <Label htmlFor={addressId}>
                        {t("ingressPage.settings.vip.form.address")}
                      </Label>
                      <Input
                        id={addressId}
                        className="max-md:h-11 font-mono"
                        value={address}
                        placeholder="192.168.1.50/24"
                        autoComplete="off"
                        spellCheck={false}
                        aria-describedby={`${addressId}-help`}
                        onChange={(e) => setAddress(e.target.value)}
                        onKeyDown={enterChecks}
                      />
                      <p id={`${addressId}-help`} className="text-xs text-muted-foreground">
                        {t("ingressPage.settings.vip.form.addressHelper")}
                      </p>
                    </div>
                    <div className="flex flex-col gap-1.5">
                      <Label htmlFor={routerIdId}>
                        {t("ingressPage.settings.vip.form.routerId")}
                      </Label>
                      <Input
                        id={routerIdId}
                        type="number"
                        min={ROUTER_ID_MIN}
                        max={ROUTER_ID_MAX}
                        className="max-md:h-11"
                        value={routerId}
                        aria-invalid={routerValue === null}
                        aria-describedby={`${routerIdId}-help`}
                        onChange={(e) => setRouterId(e.target.value)}
                        onKeyDown={enterChecks}
                      />
                      <p id={`${routerIdId}-help`} className="text-xs text-muted-foreground">
                        {t("ingressPage.settings.vip.form.routerIdHelper")}
                      </p>
                    </div>
                  </div>
                ),
              }
        }
      />
    </SettingsCard>
  );
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useId, useState } from "react";
import { Trans } from "react-i18next";

import type { HealthState } from "@/api/environments";
import { ingressRefetchInterval, ingressStatusQuery } from "@/api/ingress";
import {
  type IngressSelfRoute,
  useDisableSelfRoute,
  useEnableSelfRoute,
  useSelfRouteOffPreview,
  useSelfRoutePreview,
} from "@/api/ingress-self";
import type { IngressJob } from "@/api/ingress-switches";
import { CopyableCode } from "@/components/copyable-code";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { SettingsCard } from "@/features/environments/settings-card";
import { ReviewDialog, type ReviewKind } from "@/features/ingress/review-dialog";
import { routeHeadline } from "@/features/stacks/route-state";
import { useInstanceAdmin } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { keyExists, useT } from "@/i18n/use-t";

// The card's anchor, so a link (the HTTPS page's "See the route settings") can land on it.
export const SELF_ROUTE_ANCHOR = "tierplan-own-route";

export type SelfRouteState = IngressSelfRoute["state"];

// The states in which the route is still moving towards serving, which a person is watching (UI-SPEC S1d).
const SETTLING: ReadonlySet<SelfRouteState> = new Set([
  "self_not_attached",
  "waiting_certificate",
  "pending",
]);

// selfRouteHeadline is the one map from the self route's state to its dot and word, for the card and the HTTPS page's
// line. Every state a route also has takes the route headline's own words (U-04), so the Routes tab, the stack page and
// this card cannot disagree: self_not_attached reads as the routes' "Not attached". Only "Off" is the card's own.
export function selfRouteHeadline(state: SelfRouteState): { dot: HealthState; labelKey: string } {
  switch (state) {
    case "off":
      return { dot: "pending", labelKey: "ingressPage.settings.selfRoute.state.off" };
    case "self_not_attached":
      return routeHeadline("not_attached", false);
    default:
      return routeHeadline(state, false);
  }
}

// selfRouteRefetchInterval is the card's cadence (UI-SPEC S1d): every 10 s while the route settles or a job started
// here runs, every 30 s otherwise, and nothing while the page is hidden.
export function selfRouteRefetchInterval(
  state: SelfRouteState | undefined,
  running: boolean,
  visible: boolean,
): number | false {
  return ingressRefetchInterval(running || (state !== undefined && SETTLING.has(state)), visible);
}

function pageVisible(): boolean {
  return document.visibilityState === "visible";
}

// usingRoute says whether this page was loaded through the route's own hostname, the one case where turning it off
// may cut the page off (D-11).
export function usingRoute(host: string): boolean {
  return host !== "" && window.location.host.toLowerCase() === host.toLowerCase();
}

const mono = <span className="font-mono break-all" />;

// selfNetworkLeftOff says whether a switch-on job ended with TierPlan still off the tierplan-self network: its
// self_network step reports attached false (a timeout reports no step result, which is not this).
function selfNetworkLeftOff(job: IngressJob): boolean {
  return (job.items ?? []).some(
    (item) => item.id === "self_network" && item.result?.attached === "false",
  );
}

// SelfRouteCard is the Settings tab's "TierPlan's own route" (D-16, UI-SPEC S1): TierPlan itself served on a hostname
// through this environment's ingress. It is drawn only where the ingress status carries selfRoute, which the server sends
// for the environment TierPlan runs in or when it cannot tell; elsewhere nothing is drawn. Everyone who reads the page
// sees the whole card; only an instance Admin with ingress.manage can switch it (D-17), and everyone else is told why.
// The direct address that keeps working is always in view, and turning the route off is never held back by its state
// (D-11).
export function SelfRouteCard({ envId }: { envId: string }) {
  const t = useT();
  const id = useId();
  // True while a job started from this card, in this session, runs.
  const [running, setRunning] = useState(false);
  const status = useQuery({
    ...ingressStatusQuery(envId),
    refetchInterval: (query) =>
      selfRouteRefetchInterval(query.state.data?.selfRoute?.state, running, pageVisible()),
  });
  const needs = useNeeds("ingress.manage", envId);
  const instanceAdmin = useInstanceAdmin();
  const [reviewing, setReviewing] = useState<ReviewKind | null>(null);
  // The dialog keeps the kind it was opened for until it has gone, so its words do not change while it fades out.
  const [shown, setShown] = useState<ReviewKind>("selfEnable");
  // The hostname a switch-off was opened for, so the dialog's sentence does not change when the job releases it.
  const [leaving, setLeaving] = useState("");
  const [hostname, setHostname] = useState("");
  const preview = useSelfRoutePreview(envId, hostname);
  const enable = useEnableSelfRoute(envId, hostname);
  const offPreview = useSelfRouteOffPreview(envId);
  const disable = useDisableSelfRoute(envId);

  const title = t("ingressPage.settings.selfRoute.title");
  const data = status.data;
  if (!data) {
    if (status.isError) {
      return (
        <SettingsCard title={title} id={SELF_ROUTE_ANCHOR}>
          <Alert variant="destructive">
            <AlertDescription className="flex flex-wrap items-center gap-3">
              <span>{t("ingressPage.settings.selfRoute.error")}</span>
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="max-md:h-11"
                disabled={status.isFetching}
                onClick={() => void status.refetch()}
              >
                {t("ingressPage.settings.selfRoute.retry")}
              </Button>
            </AlertDescription>
          </Alert>
        </SettingsCard>
      );
    }
    return (
      <SettingsCard title={title} id={SELF_ROUTE_ANCHOR}>
        <Skeleton className="h-16" />
      </SettingsCard>
    );
  }

  const self = data.selfRoute;
  // TierPlan runs in another environment: this one has no card at all, and no note about it.
  if (!self) return null;

  const on = self.on;
  // The server's empty direct address means the page's own origin.
  const direct = self.directAddress !== "" ? self.directAddress : window.location.origin;
  // A route that is not on reads "Off", even when the server cannot tell whether TierPlan runs here.
  const state: SelfRouteState = on ? self.state : "off";
  const headline = selfRouteHeadline(state);
  const standalone = self.kind === "standalone";
  const ingressOn = data.enabled;

  // A permission reason comes first, ingress.manage before the instance rule (UI-SPEC S1d); then the page's own rule
  // that the route is served by the proxies. Turning the route off is never held back by anything but a permission.
  const permission = !needs.allowed
    ? needs.reason
    : !instanceAdmin
      ? t("needs.instance")
      : undefined;
  const reason =
    permission ??
    (!on && !ingressOn ? t("ingressPage.settings.selfRoute.needsIngress") : undefined);

  function detail(): string | null {
    switch (state) {
      case "pending":
        return t("ingressPage.settings.selfRoute.detail.pending");
      case "waiting_certificate": {
        const second = t("ingressPage.settings.selfRoute.detail.waitingCertificate", {
          host: self?.host,
          direct,
        });
        const reasonKey = `stackIngress.routes.reason.${self?.reason}`;
        return self?.reason && keyExists(reasonKey) ? `${t(reasonKey)} ${second}` : second;
      }
      case "self_not_attached":
        return standalone
          ? t("ingressPage.settings.selfRoute.detail.notAttachedStandalone")
          : t("ingressPage.settings.selfRoute.detail.notAttachedSwarm", { host: self?.host });
      case "unknown":
        return t("ingressPage.settings.selfRoute.detail.unknown");
      default:
        return null;
    }
  }
  const detailLine = detail();

  function begin(kind: ReviewKind) {
    if ((kind === "selfDisable" ? permission : reason) !== undefined) return;
    if (kind === "selfEnable") setHostname("");
    if (kind === "selfDisable") setLeaving(self?.host ?? "");
    setShown(kind);
    setReviewing(kind);
  }

  const hostId = `${id}-hostname`;
  const canCheck = hostname.trim() !== "";
  const check = () => {
    if (canCheck && !preview.isPending) preview.mutate();
  };
  const snippet = on && state === "self_not_attached" && !standalone ? self.snippet : undefined;
  const refusal = self.refusal;
  const refusalKey = refusal ? `ingressPage.settings.selfRoute.refusal.${refusal.code}` : "";
  const buttonKind: ReviewKind = on ? "selfDisable" : "selfEnable";
  const buttonReason = on ? permission : reason;

  return (
    <SettingsCard title={title} id={SELF_ROUTE_ANCHOR}>
      <div className="flex flex-col gap-1">
        <StatusDot state={headline.dot} label={t(headline.labelKey)} className="text-sm" />
        {detailLine ? (
          <p className="text-xs break-words text-muted-foreground">{detailLine}</p>
        ) : null}
      </div>
      {on ? null : <p className="text-sm">{t("ingressPage.settings.selfRoute.offBody")}</p>}
      <dl className="grid gap-x-6 gap-y-2 text-sm md:grid-cols-[max-content_1fr]">
        {on ? (
          <>
            <dt className="text-xs text-muted-foreground">
              {t("ingressPage.settings.selfRoute.facts.hostname")}
            </dt>
            <dd className="font-mono text-xs break-all">{self.host}</dd>
          </>
        ) : null}
        <dt className="text-xs text-muted-foreground">
          {t("ingressPage.settings.selfRoute.facts.direct")}
        </dt>
        <dd className="flex flex-col gap-1">
          <span className="font-mono text-xs break-all">{direct}</span>
          <span className="text-xs text-muted-foreground">
            {t("ingressPage.settings.selfRoute.facts.directHelp")}
          </span>
        </dd>
        {on ? (
          <>
            <dt className="text-xs text-muted-foreground">
              {t("ingressPage.settings.selfRoute.facts.network")}
            </dt>
            <dd className="flex flex-wrap items-baseline gap-x-2">
              <span className="font-mono text-xs break-all">{self.network}</span>
              <span className="text-xs">
                {t(
                  self.attached
                    ? "ingressPage.settings.selfRoute.facts.attached"
                    : "ingressPage.settings.selfRoute.facts.notAttached",
                )}
              </span>
            </dd>
          </>
        ) : null}
      </dl>
      {snippet ? (
        <Alert>
          <AlertTitle>{t("ingressPage.review.snippet.title")}</AlertTitle>
          <AlertDescription className="flex min-w-0 flex-col gap-2">
            <p className="break-words">
              {t(`ingressPage.settings.selfRoute.snippet.${snippet.kind}`)}
            </p>
            <div className="w-full min-w-0">
              <CopyableCode
                code={snippet.text}
                label={t(`ingressPage.review.snippet.${snippet.kind}`)}
              />
            </div>
            {snippet.kind === "stack_file" ? (
              <p className="text-xs break-words text-muted-foreground">
                {t("ingressPage.review.snippet.stackFileNote")}
              </p>
            ) : null}
            <p className="text-xs break-words text-muted-foreground">
              {t("ingressPage.review.snippet.note", { direct })}
            </p>
          </AlertDescription>
        </Alert>
      ) : null}
      {refusal ? (
        <Alert variant="warning">
          <AlertDescription className="break-words">
            {keyExists(refusalKey) ? (
              <span>
                <Trans
                  i18nKey={refusalKey}
                  ns="ingressPage"
                  values={{ direct }}
                  components={{ mono }}
                />
              </span>
            ) : (
              refusal.message
            )}
          </AlertDescription>
        </Alert>
      ) : null}
      {on && usingRoute(self.host) ? (
        <Alert>
          <AlertDescription className="break-words">
            <span>
              <Trans
                i18nKey="ingressPage.settings.selfRoute.usingRoute"
                ns="ingressPage"
                values={{ direct }}
                components={{ mono }}
              />
            </span>
          </AlertDescription>
        </Alert>
      ) : null}
      {on ? (
        <p className="text-xs text-muted-foreground">
          {t("ingressPage.settings.selfRoute.signIn")}
        </p>
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
            {t("ingressPage.settings.selfRoute.showProgress")}
          </Button>
        </div>
      ) : null}
      <div className="flex flex-col gap-1">
        <div className="flex flex-wrap gap-2">
          <NeedsTooltip reason={buttonReason}>
            <Button
              type="button"
              variant="outline"
              className="max-md:h-11"
              disabled={running || (!needs.allowed && !needs.reason)}
              onClick={() => begin(buttonKind)}
            >
              {t(
                on
                  ? "ingressPage.settings.selfRoute.turnOff"
                  : "ingressPage.settings.selfRoute.turnOn",
              )}
            </Button>
          </NeedsTooltip>
        </div>
        <p className="text-xs text-muted-foreground">
          {t("ingressPage.settings.selfRoute.helper")}
        </p>
      </div>
      <ReviewDialog
        envId={envId}
        kind={shown}
        open={reviewing !== null}
        onOpenChange={(next) => {
          if (!next) setReviewing(null);
        }}
        onRunning={setRunning}
        preview={shown === "selfDisable" ? offPreview : preview}
        start={shown === "selfDisable" ? disable : enable}
        bodyValues={{ direct, host: leaving }}
        // On a swarm TierPlan cannot add itself to the network, so a switch-on that left it off says where the lines are
        // (UI-SPEC S1b). A standalone host connects it on the next check.
        doneNote={
          shown === "selfEnable" && !standalone
            ? (ended) =>
                selfNetworkLeftOff(ended)
                  ? t("ingressPage.review.selfEnable.doneNotAttached")
                  : undefined
            : undefined
        }
        caution={
          shown === "selfDisable" && usingRoute(leaving)
            ? t("ingressPage.settings.selfRoute.offUsingRoute", { direct })
            : undefined
        }
        form={
          shown === "selfDisable"
            ? undefined
            : {
                canCheck,
                check,
                fields: (
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor={hostId}>
                      {t("ingressPage.settings.selfRoute.form.hostname")}
                    </Label>
                    <Input
                      id={hostId}
                      className="max-md:h-11 font-mono"
                      value={hostname}
                      autoComplete="off"
                      spellCheck={false}
                      aria-describedby={`${hostId}-help`}
                      onChange={(e) => setHostname(e.target.value)}
                      onKeyDown={(event) => {
                        if (event.key !== "Enter") return;
                        event.preventDefault();
                        check();
                      }}
                    />
                    <p id={`${hostId}-help`} className="text-xs text-muted-foreground">
                      <Trans
                        i18nKey="ingressPage.settings.selfRoute.form.hostnameHelper"
                        ns="ingressPage"
                        components={{
                          domains: (
                            <Link
                              to="/env/$envId/ingress/domains"
                              params={{ envId }}
                              className="rounded-sm text-accent-ink hover:underline"
                            />
                          ),
                        }}
                      />
                    </p>
                  </div>
                ),
              }
        }
      />
    </SettingsCard>
  );
}

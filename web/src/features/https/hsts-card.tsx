// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Check, Minus, TriangleAlert } from "lucide-react";
import { useId, useState } from "react";

import { HSTSError, httpsStatusQuery, isLockProblem, useSaveHSTS } from "@/api/https";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { SettingsCard } from "@/features/environments/settings-card";
import { type HstsStatusKey, type HstsView, hstsView } from "@/features/https/card-state";
import { formatDate } from "@/features/https/copy";
import { HstsRamp } from "@/features/https/hsts-ramp";
import { LockedNote } from "@/features/https/locked-note";
import { trustReasonText } from "@/features/https/trust-copy";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { formatAbsolute } from "@/lib/format";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The one place this card is red: a status that cannot be read or a save that failed. No HSTS state is
// ever destructive; a withdrawal or a not-sent state is a warning because TierPlan is working as designed.
const FAILURE_ALERT = { variant: "destructive" } as const;

const HELPER_ID = "hsts-helper";
// The two lock lines of the Switch while TIERPLAN_HSTS fixes the setting.
export const HSTS_LOCK_ID = "hsts-lock";

// HstsCard is the HSTS card of Settings > HTTPS (TLS-06, SET-01, SET-02, D-10 to D-14, D-22, D-23,
// D-25): a Switch that only changes a draft, an explicit Save HSTS setting, and a Status row that always
// says what TierPlan is doing with Strict-Transport-Security. hstsView reads the status into everything
// drawn here, so the card works out nothing of its own. A build without the HSTS policy has no card.
export function HstsCard() {
  const t = useT();
  const status = useQuery(httpsStatusQuery);
  const view = status.data ? hstsView(status.data) : undefined;
  if (status.data && !view) return null;

  return (
    <SettingsCard id="hsts" title={t("https.hsts.title")}>
      <p className="text-sm text-muted-foreground">{t("https.hsts.description")}</p>
      {status.isPending ? <HstsLoading /> : null}
      {status.isError ? (
        <Alert {...FAILURE_ALERT}>
          <AlertDescription>{t("common.errors.unreachable")}</AlertDescription>
        </Alert>
      ) : null}
      {view ? <HstsBody view={view} /> : null}
    </SettingsCard>
  );
}

// The card while the status is not there yet: the Switch is off, so nothing can be saved before it is
// known what it would change.
function HstsLoading() {
  const t = useT();
  const id = useId();
  return (
    <div className="flex flex-col gap-4" data-testid="hsts-loading" aria-busy="true">
      <div className="flex items-center gap-2 max-md:min-h-11">
        <Switch id={id} disabled />
        <Label htmlFor={id}>{t("https.hsts.switch")}</Label>
      </div>
      <Skeleton className="h-5 w-64" />
      <Skeleton className="h-5 w-full" />
      <Skeleton className="h-5 w-48" />
    </div>
  );
}

function HstsBody({ view }: { view: HstsView }) {
  const t = useT();
  const switchId = useId();
  const canWrite = usePermission("settings.write");
  const save = useSaveHSTS();
  const [draft, setDraft] = useState<boolean | undefined>();
  const [failed, setFailed] = useState(false);

  // TIERPLAN_HSTS fixes the setting (D-10): the Switch shows what the variable says, nothing can be
  // saved, and the lock says which variable and what is kept underneath.
  const lock = view.lock;
  const locked = lock !== undefined;
  const checked = locked ? view.enabled : (draft ?? view.storedEnabled);
  // Save waits for a change: the draft must differ from the choice kept in the database.
  const dirty = !locked && checked !== view.storedEnabled;
  const saving = save.isPending;

  const submit = () => {
    if (!canWrite || locked || !dirty || saving) return;
    setFailed(false);
    const enabled = checked;
    save.mutate(enabled, {
      onSuccess: () => {
        setDraft(undefined);
        toastSuccess(t(enabled ? "https.hsts.onDone" : "https.hsts.offDone"));
      },
      onError: (error) => {
        // A variable now fixes the setting: the status is read again (by the mutation) and the Switch
        // redraws locked. The draft was never applied, so it goes, and nothing is told as a failure.
        if (isLockProblem(error)) {
          setDraft(undefined);
          return;
        }
        setFailed(true);
      },
    });
  };

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-col gap-4">
        <div className="flex items-center gap-2 max-md:min-h-11">
          <Switch
            id={switchId}
            checked={checked}
            onCheckedChange={setDraft}
            disabled={!canWrite || locked || saving}
            aria-describedby={locked ? `${HELPER_ID} ${HSTS_LOCK_ID}` : HELPER_ID}
          />
          <Label htmlFor={switchId}>{t("https.hsts.switch")}</Label>
        </div>
        <p id={HELPER_ID} className="text-sm text-muted-foreground">
          {t("https.hsts.helper")}
        </p>
        {lock ? <LockedNote id={HSTS_LOCK_ID} lock={lock} /> : null}
        {!locked && checked && !view.storedEnabled ? (
          <p className="text-xs text-muted-foreground">{t("https.hsts.draftOn")}</p>
        ) : null}
        {!locked && !checked && view.storedEnabled ? (
          <p className="text-xs text-muted-foreground">{t("https.hsts.draftOff")}</p>
        ) : null}

        {failed ? (
          <Alert {...FAILURE_ALERT}>
            <AlertDescription>
              {save.error instanceof HSTSError && save.error.type === "https_unavailable"
                ? t("common.errors.unreachable")
                : t("common.mutationFailed")}
            </AlertDescription>
          </Alert>
        ) : null}

        {canWrite && !locked ? (
          <div className="flex flex-wrap gap-2">
            <Button
              type="button"
              variant="outline"
              aria-disabled={!dirty || saving}
              className={`${TOUCH_HEIGHT} max-md:w-full aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
              onClick={submit}
            >
              {saving ? (
                <>
                  <Spinner aria-hidden="true" role="presentation" />
                  {t("https.hsts.saving")}
                </>
              ) : (
                t("https.hsts.save")
              )}
            </Button>
          </div>
        ) : null}
      </div>

      <PortNotice view={view} />
      <StatusRows view={view} />
      <WithdrawalNotice view={view} />
      <HstsRamp view={view} />

      {canWrite ? null : (
        <p className="text-xs text-muted-foreground">{t("settings.general.readOnly")}</p>
      )}
    </div>
  );
}

const STATUS_ICON: Record<HstsStatusKey, { Icon: typeof Check; className: string }> = {
  sending: { Icon: Check, className: "text-status-online-text" },
  off_clearing: { Icon: Minus, className: "text-muted-foreground" },
  off_silent: { Icon: Minus, className: "text-muted-foreground" },
  not_sent: { Icon: TriangleAlert, className: "text-status-warning-text" },
  not_sent_clearing: { Icon: TriangleAlert, className: "text-status-warning-text" },
  withdrawn: { Icon: TriangleAlert, className: "text-status-warning-text" },
};

// The Status row and what goes with it (UI-SPEC "Status"): a dl, as the Certificate in use card.
function StatusRows({ view }: { view: HstsView }) {
  const t = useT();
  const { Icon, className } = STATUS_ICON[view.state];
  return (
    <dl className="grid grid-cols-1 gap-x-6 gap-y-4 md:grid-cols-[160px_1fr]">
      <dt className="text-sm font-medium text-muted-foreground">{t("https.hsts.status.label")}</dt>
      <dd className="min-w-0 text-sm">
        <div className="flex flex-col gap-1">
          <span className="flex items-start gap-2 font-medium">
            <Icon className={`mt-0.5 size-4 shrink-0 ${className}`} aria-hidden="true" />
            <span className="min-w-0">{t(`https.hsts.status.${view.state}.value`)}</span>
          </span>
          <span className="break-words text-xs text-muted-foreground">
            {t(`https.hsts.status.${view.state}.extra`)}
          </span>
        </div>
      </dd>

      {view.state === "sending" ? (
        <>
          <dt className="text-sm font-medium text-muted-foreground">
            {t("https.hsts.status.lifetime")}
          </dt>
          <dd className="min-w-0 text-sm">
            <div className="flex flex-col gap-1">
              <code className="break-all font-mono text-xs">
                {t("https.hsts.lifetimeValue", { seconds: view.maxAge })}
              </code>
              <span className="text-xs text-muted-foreground">
                {t("https.hsts.stepOf", {
                  n: view.step,
                  label: t(`https.hsts.steps.label.${view.step}`),
                })}
              </span>
            </div>
          </dd>
        </>
      ) : null}

      {view.next ? (
        <>
          <dt className="text-sm font-medium text-muted-foreground">
            {t("https.hsts.status.next")}
          </dt>
          <dd className="min-w-0 text-sm">
            {view.next.kind === "at" ? (
              <div className="flex flex-col gap-1">
                <span className="break-words">{formatAbsolute(view.next.at)}</span>
                <span className="text-xs text-muted-foreground">{t("https.hsts.nextAfter")}</span>
              </div>
            ) : (
              <span>
                {t(view.next.kind === "final" ? "https.hsts.nextFinal" : "https.hsts.nextCapped")}
              </span>
            )}
          </dd>
        </>
      ) : null}

      {view.reason ? <WhyRow reason={view.reason} host={view.host} /> : null}
    </dl>
  );
}

// The Why row of a state that is on but not sent: the trust reason and its fix, worded by the one table
// the certificate-files block and the install dialog use (E9).
function WhyRow({ reason, host }: { reason: string; host: string | undefined }) {
  const t = useT();
  const text = trustReasonText(t, reason, host);
  return (
    <>
      <dt className="text-sm font-medium text-muted-foreground">{t("https.hsts.status.why")}</dt>
      <dd className="min-w-0 text-sm">
        <div className="flex flex-col gap-1">
          <span className="break-words">{text.sentence}</span>
          <span className="break-words text-xs text-muted-foreground">{text.fix}</span>
        </div>
      </dd>
    </>
  );
}

// PortNotice warns, whether HSTS is on or off, that the public URL is on a port other than 443 while
// browsers apply HSTS to the whole name (WR-04). It is a warning only: HSTS may still be turned on.
function PortNotice({ view }: { view: HstsView }) {
  const t = useT();
  if (!view.port) return null;
  const { port, host } = view.port;
  return (
    <Alert variant="warning" role="note" data-testid="hsts-port-notice">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>{t("https.hsts.port.title")}</AlertTitle>
      <AlertDescription className="break-words">
        {host
          ? t("https.hsts.port.body", { port, host })
          : t("https.hsts.port.bodyNoHost", { port })}
      </AlertDescription>
    </Alert>
  );
}

// WithdrawalNotice says, while HSTS is withdrawn ahead of the certificate's end, why browsers are told
// to forget it while the certificate still works and when it comes back (D-12). A warning, not an
// error: TierPlan is working as designed.
function WithdrawalNotice({ view }: { view: HstsView }) {
  const t = useT();
  if (view.state !== "withdrawn" || !view.withdrawal) return null;
  const source = view.withdrawal.source === "cert_files" ? "cert_files" : "lets_encrypt";
  return (
    <Alert variant="warning" role="note">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>{t(`https.hsts.withdrawn.${source}.title`)}</AlertTitle>
      <AlertDescription>
        {t(`https.hsts.withdrawn.${source}.body`, { date: formatDate(view.withdrawal.notAfter) })}
      </AlertDescription>
    </Alert>
  );
}

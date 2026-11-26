// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { TriangleAlert } from "lucide-react";
import { useState } from "react";

import {
  type IngressAccount,
  IngressAccountError,
  ingressAccountQuery,
  useAcceptIngressTerms,
  useResumeIngressCertificates,
} from "@/api/ingress-account";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { useNow } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { SettingsCard } from "@/features/environments/settings-card";
import { useInstanceAdmin } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { formatRelative } from "@/lib/format";
import { httpsUrl } from "@/lib/https-url";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// IngressAccountCard is the "Ingress certificates" card of Settings > HTTPS (Phase 27, D-04, UI-SPEC
// S6b): whether an Admin accepted Let's Encrypt's terms for this install, which no ingress certificate
// is ordered without, and the pause a restore puts on certificate work. The account is the install's, not
// an environment's, and its routes need the global settings.* permissions that only instance admins hold,
// so the card is drawn for an instance admin and for nobody else, and nobody else's page asks for it.
export function IngressAccountCard() {
  const instanceAdmin = useInstanceAdmin();
  if (!instanceAdmin) return null;
  return <IngressAccountSection />;
}

function IngressAccountSection() {
  const t = useT();
  const account = useQuery(ingressAccountQuery);

  return (
    <SettingsCard id="ingress-certificates" title={t("https.ingressAccount.title")}>
      {account.isPending ? (
        <Skeleton className="h-24 w-full" data-testid="ingress-account-loading" />
      ) : null}
      {account.isError ? (
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("https.ingressAccount.loadFailed")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={account.isFetching}
              onClick={() => void account.refetch()}
            >
              {t("https.ingressAccount.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      {account.data ? <IngressAccountBody account={account.data} /> : null}
    </SettingsCard>
  );
}

function IngressAccountBody({ account }: { account: IngressAccount }) {
  const t = useT();
  const now = useNow();
  const needs = useNeeds("settings.write");
  const [asking, setAsking] = useState<"accept" | "resume" | null>(null);
  const accepted = account.termsAccepted;
  const when = account.termsAcceptedAt
    ? formatRelative(account.termsAcceptedAt, new Date(now), t)
    : "";
  // The server names a terms address only when the directory advertised one just now. The address comes from the CA, so it
  // is linked only when it is an https: address; any other is shown as plain text, never put in an href.
  const links = (account.directories ?? [])
    .filter((d) => d.termsOfService !== "")
    .map((d) => ({ issuer: d.issuer, href: httpsUrl(d.termsOfService) }));
  const reasonKey =
    account.pausedReason === "restore"
      ? "https.ingressAccount.pausedRestore"
      : "https.ingressAccount.paused";

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-1">
        <StatusDot
          state={accepted ? "online" : "degraded"}
          label={
            accepted
              ? t("https.ingressAccount.accepted", { time: when }).trim()
              : t("https.ingressAccount.notAccepted")
          }
        />
        {accepted ? null : (
          <p className="text-xs text-muted-foreground">
            {t("https.ingressAccount.notAcceptedNote")}
          </p>
        )}
      </div>
      <p className="break-words text-sm">{t("https.ingressAccount.body")}</p>
      {links.length > 0 ? (
        <ul className="flex flex-col gap-1 text-sm">
          {links.map((d) => (
            <li key={d.issuer}>
              {d.href ? (
                <a
                  href={d.href}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="rounded-sm underline underline-offset-3 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
                >
                  {t("https.ingressAccount.termsLink", { issuer: d.issuer })}
                </a>
              ) : (
                <span>{t("https.ingressAccount.termsLink", { issuer: d.issuer })}</span>
              )}
            </li>
          ))}
        </ul>
      ) : null}
      {account.paused ? (
        <Alert variant="warning">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription>{t(reasonKey, { reason: account.pausedReason })}</AlertDescription>
        </Alert>
      ) : null}
      {accepted && !account.paused ? null : (
        <div className="flex flex-wrap gap-3">
          {accepted ? null : (
            <ActionButton
              needs={needs}
              onOpen={() => setAsking("accept")}
              label={t("https.ingressAccount.accept.action")}
            />
          )}
          {account.paused ? (
            <ActionButton
              needs={needs}
              variant="outline"
              onOpen={() => setAsking("resume")}
              label={t("https.ingressAccount.resume.action")}
            />
          ) : null}
        </div>
      )}
      <ConfirmDialog kind="accept" open={asking === "accept"} onClose={() => setAsking(null)} />
      <ConfirmDialog kind="resume" open={asking === "resume"} onClose={() => setAsking(null)} />
    </div>
  );
}

// ActionButton opens a confirmation. Without settings.write it stays in the tab order, looks disabled
// and says what it needs (D-19); the server enforces the permission whatever this draws.
function ActionButton({
  needs,
  label,
  variant,
  onOpen,
}: {
  needs: ReturnType<typeof useNeeds>;
  label: string;
  variant?: "outline";
  onOpen: () => void;
}) {
  return (
    <NeedsTooltip reason={needs.allowed ? undefined : needs.reason}>
      <Button
        type="button"
        variant={variant}
        className={TOUCH_HEIGHT}
        disabled={!needs.allowed && !needs.reason}
        onClick={onOpen}
      >
        {label}
      </Button>
    </NeedsTooltip>
  );
}

// ConfirmDialog is "Accept Let's Encrypt's terms?" and "Resume certificate work?". Nothing closes it
// while the request is in flight. A refusal stays in the dialog as the server's own sentence (a destructive
// Alert, never a toast); a success closes it once the account has been read again.
function ConfirmDialog({
  kind,
  open,
  onClose,
}: {
  kind: "accept" | "resume";
  open: boolean;
  onClose: () => void;
}) {
  const t = useT();
  const accept = useAcceptIngressTerms();
  const resume = useResumeIngressCertificates();
  const mutation = kind === "accept" ? accept : resume;
  const [refusal, setRefusal] = useState<string | null>(null);
  const prefix = `https.ingressAccount.${kind}`;
  const pending = mutation.isPending;

  function change(next: boolean) {
    if (pending) return;
    if (!next) {
      setRefusal(null);
      onClose();
    }
  }

  async function confirm() {
    if (pending) return;
    setRefusal(null);
    try {
      await mutation.mutateAsync();
    } catch (error) {
      setRefusal(
        error instanceof IngressAccountError && error.detail
          ? error.detail
          : t("common.mutationFailed"),
      );
      return;
    }
    onClose();
  }

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{t(`${prefix}.title`)}</AlertDialogTitle>
          <AlertDialogDescription>{t(`${prefix}.body`)}</AlertDialogDescription>
        </AlertDialogHeader>
        {refusal !== null ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{refusal}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t(`${prefix}.cancel`)}
          </AlertDialogCancel>
          <Button
            type="button"
            aria-disabled={pending}
            className={`${TOUCH_HEIGHT} aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={() => void confirm()}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" role="presentation" />
                {t(`${prefix}.pending`)}
              </>
            ) : (
              t(`${prefix}.confirm`)
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

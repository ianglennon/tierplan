// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { useRef, useState } from "react";
import { Trans } from "react-i18next";

import { IngressActionError } from "@/api/ingress";
import {
  ingressJoinTokenQuery,
  useDisableIngressJoinToken,
  useIssueIngressJoinToken,
} from "@/api/ingress-switches";
import { CopyableCode } from "@/components/copyable-code";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
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
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The three things a person does to the token. Each has its own words under ingressPage.settings.joinToken.<kind>.
type TokenKind = "rotate" | "make" | "disable";

// IngressJoinTokenCard is the Settings tab's fourth card (UI-SPEC S6 card 4, D-05, U-20): which join token the
// ingress proxies enrol with, when it was made and, for someone who may manage ingress, by whom (A-04). The token
// itself is never drawn here. Rotating or making one shows the clear token once, in the dialog that did it, and it is
// gone when that dialog closes (T-27-72). The ingress proxy has no install command to show: TierPlan's deployer runs
// it with the token as a swarm secret or a file, so there is nothing for a person to paste (U-20). A reader sees the
// status and the buttons disabled with the reason.
export function IngressJoinTokenCard({ envId }: { envId: string }) {
  const t = useT();
  return (
    // The anchor the revoke notice links to ("Go to the join token").
    <SettingsCard title={t("ingressPage.settings.joinToken.title")} id="join-token">
      <TokenBody envId={envId} />
    </SettingsCard>
  );
}

// TokenBody is what the card holds: the status read, then the status line and the buttons.
function TokenBody({ envId }: { envId: string }) {
  const t = useT();
  const status = useQuery(ingressJoinTokenQuery(envId));
  const needs = useNeeds("ingress.manage", envId);
  const [kind, setKind] = useState<TokenKind | null>(null);
  // The dialog keeps the kind it was opened for until it has gone, so its words do not change while it fades out.
  const [shown, setShown] = useState<TokenKind>("rotate");

  if (status.isPending) return <Skeleton className="h-16" />;
  if (status.isError) {
    return (
      <>
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("ingressPage.settings.joinToken.error")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={status.isFetching}
              onClick={() => void status.refetch()}
            >
              {t("ingressPage.settings.joinToken.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      </>
    );
  }

  const data = status.data;
  const active = data.exists && !data.disabled;
  // Anyone who reads the page sees whether a token is active; only a manager is told who made it, and the server
  // leaves the name blank for the rest, so there is nothing to compare here: a blank name draws no "by".
  const by = data.createdBy;

  function begin(next: TokenKind) {
    if (!needs.allowed) return;
    setShown(next);
    setKind(next);
  }

  function button(next: TokenKind) {
    return (
      <NeedsTooltip reason={needs.reason}>
        <Button type="button" variant="outline" className="max-md:h-11" onClick={() => begin(next)}>
          {t(`ingressPage.settings.joinToken.${next}.button`)}
        </Button>
      </NeedsTooltip>
    );
  }

  return (
    <>
      <div className="flex flex-col gap-1">
        {active ? (
          <StatusDot
            state="online"
            className="text-sm"
            label={
              <Trans
                i18nKey={
                  by !== ""
                    ? "ingressPage.settings.joinToken.activeBy"
                    : "ingressPage.settings.joinToken.active"
                }
                ns="ingressPage"
                values={{ user: by }}
                components={{ time: <RelativeTime value={data.createdAt ?? ""} /> }}
              />
            }
          />
        ) : (
          <StatusDot
            state={data.exists ? "degraded" : "pending"}
            className="text-sm"
            label={t(
              data.exists
                ? "ingressPage.settings.joinToken.disabled"
                : "ingressPage.settings.joinToken.none",
            )}
          />
        )}
      </div>
      <div className="flex flex-wrap gap-2">
        {active ? (
          <>
            {button("rotate")}
            {button("disable")}
          </>
        ) : (
          button("make")
        )}
      </div>
      <TokenDialog envId={envId} kind={shown} open={kind !== null} onClose={() => setKind(null)} />
    </>
  );
}

interface TokenDialogProps {
  envId: string;
  kind: TokenKind;
  open: boolean;
  onClose: () => void;
}

// TokenDialog is the confirmation of each of the three: the dialog states what happens and what stays as it is, the
// confirm does it, and a refusal is drawn in the dialog as the server's sentence. When a token was made, the dialog
// shows it once and has one button; the token lives in this dialog's own state and in nothing else, so closing it
// drops the only copy the browser held.
function TokenDialog({ envId, kind, open, onClose }: TokenDialogProps) {
  const t = useT();
  const issue = useIssueIngressJoinToken(envId);
  const disable = useDisableIngressJoinToken(envId);
  const [token, setToken] = useState<string | null>(null);
  // A second press can land before the pending state has rendered.
  const sending = useRef(false);

  const pending = issue.isPending || disable.isPending;
  const error = kind === "disable" ? disable.error : issue.error;
  const detail =
    error instanceof IngressActionError && error.detail ? error.detail : t("common.mutationFailed");

  // Closing drops the token, the only copy the browser held.
  function finish() {
    setToken(null);
    issue.reset();
    disable.reset();
    onClose();
  }

  function change(next: boolean) {
    if (!next && pending) return;
    if (!next) finish();
  }

  function confirm() {
    if (sending.current || pending) return;
    sending.current = true;
    const settled = () => {
      sending.current = false;
    };
    if (kind === "disable") {
      disable.mutate(undefined, { onSettled: settled, onSuccess: finish });
      return;
    }
    issue.mutate(undefined, {
      onSettled: settled,
      onSuccess: (made) => {
        setToken(made.token);
        // The mutation's own copy of the answer is not needed: the dialog holds the token until it closes.
        issue.reset();
      },
    });
  }

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent>
        {open ? (
          token !== null ? (
            <>
              <AlertDialogHeader>
                <AlertDialogTitle>
                  {t(`ingressPage.settings.joinToken.${kind}.doneTitle`)}
                </AlertDialogTitle>
                <AlertDialogDescription>
                  {t("ingressPage.settings.joinToken.once")}
                </AlertDialogDescription>
              </AlertDialogHeader>
              <CopyableCode code={token} label={t("ingressPage.settings.joinToken.label")} />
              <AlertDialogFooter>
                <AlertDialogCancel className={TOUCH_HEIGHT}>
                  {t("ingressPage.review.close")}
                </AlertDialogCancel>
              </AlertDialogFooter>
            </>
          ) : (
            <>
              <AlertDialogHeader>
                <AlertDialogTitle>
                  {t(`ingressPage.settings.joinToken.${kind}.title`)}
                </AlertDialogTitle>
                <AlertDialogDescription>
                  {t(`ingressPage.settings.joinToken.${kind}.body`)}
                </AlertDialogDescription>
              </AlertDialogHeader>
              {issue.isError || disable.isError ? (
                <Alert variant="destructive" role="alert">
                  <AlertDescription className="break-words">{detail}</AlertDescription>
                </Alert>
              ) : null}
              <AlertDialogFooter>
                {/* Radix focuses the dismiss button when the dialog opens. */}
                <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
                  {t(`ingressPage.settings.joinToken.${kind}.dismiss`)}
                </AlertDialogCancel>
                <Button type="button" disabled={pending} className={TOUCH_HEIGHT} onClick={confirm}>
                  {pending ? <Spinner aria-hidden="true" /> : null}
                  {t(`ingressPage.settings.joinToken.${kind}.confirm`)}
                </Button>
              </AlertDialogFooter>
            </>
          )
        ) : null}
      </AlertDialogContent>
    </AlertDialog>
  );
}

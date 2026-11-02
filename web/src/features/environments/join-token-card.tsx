// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useRef, useState } from "react";

import { disableToken, refreshAfterAgentChange, tokenStatusQuery } from "@/api/agents";
import { RelativeTime } from "@/components/relative-time";
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
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { InstallDialog } from "@/features/environments/install-dialog";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// A character that is in no message, to cut the sentence where the relative
// time goes. The time is a component (a <time> with a tooltip), so it cannot
// be put into the string by i18next.
const SLOT = "\u0001";

interface JoinTokenCardProps {
  envId: string;
}

// JoinTokenCard is the "Join token" section (D-02, D-38): when the current
// token was made and by whom, Show install command (which rotates only when
// "Generate new token" is pressed in the dialog) and Disable token. The page
// shows it only to someone holding environments.token; the server checks again.
export function JoinTokenCard({ envId }: JoinTokenCardProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const status = useQuery(tokenStatusQuery(envId));
  const [installOpen, setInstallOpen] = useState(false);
  const showButton = useRef<HTMLButtonElement>(null);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [failed, setFailed] = useState(false);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);

  const disable = useMutation({
    mutationFn: () => disableToken(envId),
    onSuccess: async () => {
      await refreshAfterAgentChange(queryClient, envId);
      setConfirmOpen(false);
    },
    onError: () => setFailed(true),
    onSettled: () => {
      inFlight.current = false;
    },
  });

  const data = status.data;
  const [before = "", after = ""] = data
    ? t("envsettings.token.lastChanged", { when: SLOT, user: data.createdBy }).split(SLOT)
    : [];

  function changeInstall(open: boolean) {
    setInstallOpen(open);
    // The dialog's rotation re-enables enrolment and makes a new token, and it
    // does not tell this card, so the status is read again when it closes.
    if (!open) void queryClient.invalidateQueries({ queryKey: tokenStatusQuery(envId).queryKey });
  }

  function changeConfirm(open: boolean) {
    if (!open && disable.isPending) return;
    setConfirmOpen(open);
    if (open) {
      setFailed(false);
      disable.reset();
    }
  }

  return (
    <SettingsCard title={t("envsettings.sections.token")}>
      <p className="text-sm text-muted-foreground">{t("envsettings.token.body")}</p>

      {status.isPending ? (
        <div aria-busy="true">
          <span role="status" className="sr-only">
            {t("envsettings.token.loading")}
          </span>
          <Skeleton className="h-8 w-64" />
        </div>
      ) : null}

      {status.isError ? (
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("envsettings.token.loadFailed")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={status.isFetching}
              onClick={() => void status.refetch()}
            >
              {t("envsettings.token.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}

      {data?.disabled ? (
        <div className="flex flex-col gap-2">
          <div>
            <Badge variant="secondary">{t("envsettings.token.disabledBadge")}</Badge>
          </div>
          <p className="text-sm">{t("envsettings.token.disabledBody")}</p>
        </div>
      ) : null}

      {data && !data.disabled ? (
        <p className="text-sm">
          {before}
          <RelativeTime value={data.createdAt} />
          {after}
        </p>
      ) : null}

      <div className="flex flex-wrap gap-2">
        <Button
          type="button"
          variant="outline"
          className={`${TOUCH_HEIGHT} max-md:flex-1`}
          ref={showButton}
          onClick={() => setInstallOpen(true)}
        >
          {t("envsettings.token.show")}
        </Button>
        {data && !data.disabled ? (
          <Button
            type="button"
            variant="outline"
            className={`${TOUCH_HEIGHT} max-md:flex-1`}
            onClick={() => changeConfirm(true)}
          >
            {t("envsettings.token.disable")}
          </Button>
        ) : null}
      </div>

      <InstallDialog
        open={installOpen}
        mode="rotate"
        envId={envId}
        returnFocusRef={showButton}
        onOpenChange={changeInstall}
      />

      <AlertDialog open={confirmOpen} onOpenChange={changeConfirm}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("envsettings.token.disableConfirm.title")}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("envsettings.token.disableConfirm.body")}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {failed ? (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{t("common.mutationFailed")}</AlertDescription>
            </Alert>
          ) : null}
          <AlertDialogFooter>
            {/* Radix focuses the Cancel button when the dialog opens. */}
            <AlertDialogCancel disabled={disable.isPending} className={TOUCH_HEIGHT}>
              {t("envsettings.token.disableConfirm.cancel")}
            </AlertDialogCancel>
            <Button
              type="button"
              disabled={disable.isPending}
              className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90`}
              onClick={() => {
                // Radix would close the dialog on a click; it stays open until
                // the request answers.
                if (inFlight.current) return;
                inFlight.current = true;
                setFailed(false);
                disable.mutate();
              }}
            >
              {disable.isPending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("envsettings.token.disableConfirm.pending")}
                </>
              ) : (
                t("envsettings.token.disableConfirm.confirm")
              )}
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </SettingsCard>
  );
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useRef, useState } from "react";

import { revokeUserSessions, userSessionsQuery } from "@/api/sessions";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
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
import { Spinner } from "@/components/ui/spinner";
import { SessionsTable } from "@/features/account/sessions-table";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface SessionsCardProps {
  userId: string;
  username: string;
}

// SessionsCard is where an instance admin sees a user's sessions and ends them
// all (D-23). There is no Revoke on a row: an admin ends everything or nothing,
// and the confirm says so. Ending them is audited by the server.
export function SessionsCard({ userId, username }: SessionsCardProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const query = useQuery(userSessionsQuery(userId));
  const [confirming, setConfirming] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);

  const sessions = query.data;
  const failed = query.isError && !sessions;
  const empty = sessions !== undefined && sessions.length === 0;

  const refresh = () =>
    void queryClient.invalidateQueries({ queryKey: userSessionsQuery(userId).queryKey });

  async function endAll() {
    if (inFlight.current) return;
    inFlight.current = true;
    setPending(true);
    setError(null);
    try {
      const result = await revokeUserSessions(userId);
      if (result.kind === "ok") {
        toastSuccess(t("sessions.card.done", { username }));
        setConfirming(false);
        refresh();
        return;
      }
      setError(t("sessions.card.failed", { username }));
      // Nothing changed here, but the list may have: read it again.
      refresh();
    } finally {
      inFlight.current = false;
      setPending(false);
    }
  }

  function change(open: boolean) {
    if (open || pending) return;
    setError(null);
    setConfirming(false);
  }

  return (
    <SettingsCard title={t("userpage.sections.sessions")} id="sessions">
      <p className="text-sm text-muted-foreground">{t("sessions.card.caption", { username })}</p>
      {failed ? (
        <Alert variant="destructive">
          <AlertTitle>{t("sessions.loadFailed")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("sessions.loadFailedBody")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {query.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("sessions.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : empty ? (
        <p className="text-sm break-words">{t("sessions.card.empty", { username })}</p>
      ) : (
        <SessionsTable sessions={sessions} loading={query.isPending} />
      )}
      <div>
        <Button
          type="button"
          variant="destructive"
          className={`${TOUCH_HEIGHT} max-md:w-full`}
          disabled={empty || failed || query.isPending}
          onClick={() => setConfirming(true)}
        >
          {t("sessions.card.endAll")}
        </Button>
      </div>
      <AlertDialog open={confirming} onOpenChange={change}>
        <AlertDialogContent>
          <AlertDialogHeader>
            {/* A long name wraps rather than running off the dialog. */}
            <AlertDialogTitle className="break-words">
              {t("sessions.card.title", { username })}
            </AlertDialogTitle>
            <AlertDialogDescription>{t("sessions.card.body")}</AlertDialogDescription>
          </AlertDialogHeader>
          {error ? (
            <Alert variant="destructive" role="alert">
              <AlertDescription className="break-words">{error}</AlertDescription>
            </Alert>
          ) : null}
          <AlertDialogFooter>
            {/* Radix focuses the Cancel button when the dialog opens. */}
            <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
              {t("sessions.card.cancel")}
            </AlertDialogCancel>
            <Button
              type="button"
              variant="destructive"
              disabled={pending}
              className={TOUCH_HEIGHT}
              onClick={() => void endAll()}
            >
              {pending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("sessions.card.pending")}
                </>
              ) : (
                t("sessions.card.confirm")
              )}
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </SettingsCard>
  );
}

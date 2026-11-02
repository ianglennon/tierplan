// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type RefObject, useRef, useState } from "react";

import {
  mySessionsQuery,
  revokeMyOtherSessions,
  revokeMySession,
  type SessionInfo,
} from "@/api/sessions";
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
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Spinner } from "@/components/ui/spinner";
import { SessionsTable, useDeviceName } from "@/features/account/sessions-table";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface SignOutOthersDialogProps {
  open: boolean;
  onClose: () => void;
  onDone: () => void;
  onStale: () => void;
}

// SignOutOthersDialog is "Sign out everywhere else?". Nothing happens until the
// button is pressed, it cannot be dismissed while the request is in flight, and
// a failure stays inside it.
function SignOutOthersDialog({ open, onClose, onDone, onStale }: SignOutOthersDialogProps) {
  const t = useT();
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const inFlight = useRef(false);

  async function signOut() {
    if (inFlight.current) return;
    inFlight.current = true;
    setPending(true);
    setError(null);
    try {
      const result = await revokeMyOtherSessions();
      if (result.kind === "ok") {
        toastSuccess(t("sessions.others.done"));
        onDone();
        return;
      }
      setError(t("sessions.others.failed"));
      onStale();
    } finally {
      inFlight.current = false;
      setPending(false);
    }
  }

  function change(next: boolean) {
    if (next || pending) return;
    setError(null);
    onClose();
  }

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{t("sessions.others.title")}</AlertDialogTitle>
          <AlertDialogDescription>{t("sessions.others.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("sessions.others.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            disabled={pending}
            className={TOUCH_HEIGHT}
            onClick={() => void signOut()}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("sessions.others.pending")}
              </>
            ) : (
              t("sessions.others.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

interface SessionsDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // Where focus goes when the dialog closes: the control that opened it. The
  // menu item that did is gone by then, so Radix cannot find its own way back.
  returnFocusRef?: RefObject<HTMLElement | null>;
}

// SessionsDialog is "Your sessions" from the user menu (D-23): every browser the
// user is signed in on, Revoke for each other session, and "Sign out everywhere
// else". The session being used has no Revoke; signing out is how it ends.
export function SessionsDialog({ open, onOpenChange, returnFocusRef }: SessionsDialogProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const query = useQuery({ ...mySessionsQuery, enabled: open });
  const deviceName = useDeviceName();
  const [confirmOthers, setConfirmOthers] = useState(false);
  const [revoking, setRevoking] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const inFlight = useRef(false);

  const sessions = query.data;
  const others = sessions?.filter((s) => !s.current).length ?? 0;
  const failed = query.isError && !sessions;

  const refresh = () => void queryClient.invalidateQueries({ queryKey: mySessionsQuery.queryKey });

  async function revoke(session: SessionInfo) {
    if (inFlight.current) return;
    inFlight.current = true;
    setRevoking(session.id);
    setError(null);
    try {
      const result = await revokeMySession(session.id);
      if (result.kind === "ok") toastSuccess(t("sessions.revoked"));
      else setError(t("sessions.revokeFailed"));
      // Either way the list is read again: nothing changed, or it did.
      refresh();
    } finally {
      inFlight.current = false;
      setRevoking(null);
    }
  }

  function change(next: boolean) {
    if (!next) setError(null);
    onOpenChange(next);
  }

  return (
    <Dialog open={open} onOpenChange={change}>
      <DialogContent
        className="sm:max-w-160"
        showCloseButton={false}
        onCloseAutoFocus={(event) => {
          if (!returnFocusRef?.current) return;
          event.preventDefault();
          returnFocusRef.current.focus();
        }}
      >
        <DialogHeader>
          <DialogTitle>{t("sessions.title")}</DialogTitle>
          <DialogDescription>{t("sessions.body")}</DialogDescription>
        </DialogHeader>
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
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
        ) : (
          <>
            <SessionsTable
              sessions={sessions}
              loading={query.isPending}
              renderAction={(s) =>
                s.current ? null : (
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="max-md:h-11"
                    disabled={revoking !== null}
                    aria-label={`${t("sessions.revoke")}, ${deviceName(s)}`}
                    onClick={() => void revoke(s)}
                  >
                    {revoking === s.id ? (
                      <>
                        <Spinner aria-hidden="true" />
                        {t("sessions.revokePending")}
                      </>
                    ) : (
                      t("sessions.revoke")
                    )}
                  </Button>
                )
              }
            />
            {sessions && others === 0 ? (
              <p className="text-sm text-muted-foreground">{t("sessions.onlyHere")}</p>
            ) : null}
          </>
        )}
        <DialogFooter>
          <Button
            type="button"
            variant="secondary"
            className={TOUCH_HEIGHT}
            disabled={others === 0}
            onClick={() => setConfirmOthers(true)}
          >
            {t("sessions.others.button")}
          </Button>
          <DialogClose asChild>
            <Button type="button" variant="outline" className={TOUCH_HEIGHT}>
              {t("sessions.close")}
            </Button>
          </DialogClose>
        </DialogFooter>
        <SignOutOthersDialog
          open={confirmOthers}
          onClose={() => setConfirmOthers(false)}
          onDone={() => {
            setConfirmOthers(false);
            refresh();
          }}
          onStale={refresh}
        />
      </DialogContent>
    </Dialog>
  );
}

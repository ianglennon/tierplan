// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useRef, useState } from "react";

import { previewEnvAccessImpact, removeEnvAccess } from "@/api/envaccess";
import { type Grant, previewGrantImpact, removeGrant } from "@/api/grants";
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
import { Spinner } from "@/components/ui/spinner";
import type { GrantEnvironment, GrantSubject } from "@/features/access/grant-dialog";
import { ImpactPreview } from "@/features/access/impact-preview";
import { useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface RemoveGrantDialogProps {
  subject: GrantSubject;
  // Set when the dialog is opened from an environment's Access tab: the removal
  // and its preview then go through that environment's routes, which an env
  // Admin may use and which cannot reach any other environment.
  environment?: GrantEnvironment;
  // The grant to remove, or null while the dialog is closed.
  grant: Grant | null;
  onClose: () => void;
  // Called once the access is removed.
  onDone: () => void;
  // Called when the answer shows what the screen holds is out of date.
  onStale: () => void;
}

// RemoveGrantDialog is "Remove access?" (D-27): who loses which role where, the
// impact list ("What changes") and a destructive confirm that stays unavailable
// until the list has loaded. Focus starts on Cancel. When the signed-in user is
// removing their own Admin it says they will lose access to manage the
// environment. It cannot be dismissed while the request is in flight, and a
// failure stays inside the dialog.
export function RemoveGrantDialog({
  subject,
  environment: tab,
  grant,
  onClose,
  onDone,
  onStale,
}: RemoveGrantDialogProps) {
  const t = useT();
  const me = useMe();
  // Ready is remembered for the grant it loaded for, so it can never carry over
  // to another one.
  const previewKey = grant ? grant.id : null;
  const [readyFor, setReadyFor] = useState<string | null>(null);
  const ready = previewKey !== null && readyFor === previewKey;
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const environment = grant
    ? grant.scope === "all"
      ? t("grants.scope.all")
      : (grant.envName ?? "")
    : "";
  const ownAdmin =
    grant !== null &&
    subject.type === "user" &&
    grant.role === "admin" &&
    me.data?.user.id === subject.id;

  async function remove() {
    if (!grant || !ready || inFlight.current) return;
    inFlight.current = true;
    setPending(true);
    setError(null);
    try {
      const result = tab
        ? await removeEnvAccess(tab.id, grant.id)
        : await removeGrant(subject.type, subject.id, grant.id);
      if (result.kind === "ok") {
        toastSuccess(t("grants.remove.done"));
        onDone();
        return;
      }
      setError(result.kind === "not_found" ? t("grants.remove.gone") : t("grants.remove.failed"));
      onStale();
    } finally {
      inFlight.current = false;
      setPending(false);
    }
  }

  function change(open: boolean) {
    if (open || pending) return;
    setError(null);
    onClose();
  }

  return (
    <AlertDialog open={grant !== null} onOpenChange={change}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{t("grants.remove.title")}</AlertDialogTitle>
          {/* Names wrap rather than running off the dialog. */}
          <AlertDialogDescription className="break-words">
            {grant
              ? t("grants.remove.body", {
                  subject: subject.name,
                  role: t(`grants.role.${grant.role}.label`),
                  environment,
                })
              : null}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {ownAdmin ? (
          <Alert role="status">
            <AlertDescription>{t("grants.remove.ownAdmin", { environment })}</AlertDescription>
          </Alert>
        ) : null}
        {grant ? (
          <ImpactPreview
            key={previewKey}
            load={() =>
              tab
                ? previewEnvAccessImpact(tab.id, grant.id, { op: "delete" })
                : previewGrantImpact(subject.type, subject.id, grant.id, { op: "delete" })
            }
            onReadyChange={(r) => setReadyFor(r ? previewKey : null)}
          />
        ) : null}
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("grants.remove.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            aria-disabled={!ready || pending}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90 aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={() => void remove()}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("grants.remove.pending")}
              </>
            ) : (
              t("grants.remove.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

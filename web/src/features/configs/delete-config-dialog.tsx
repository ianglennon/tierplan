// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useRef, useState } from "react";

import { ConfigRequestError, useDeleteConfig } from "@/api/configs";
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
import { useT } from "@/i18n/use-t";
import { toastInfo, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

type Translate = (key: string, options?: Record<string, unknown>) => string;

// configDeleteReason is why Delete… is unavailable on a config, or undefined when it is
// available: who may comes first, then whether any service spec still mentions it (a service
// scaled to 0 counts). On the config's own page the words do not send the reader to the page they
// are on.
export function configDeleteReason(
  t: Translate,
  needsReason: string | undefined,
  usedBy: number,
  onOwnPage = false,
): string | undefined {
  if (needsReason) return needsReason;
  if (usedBy <= 0) return undefined;
  return onOwnPage
    ? t("configs.delete.reason", { count: usedBy })
    : t("configs.list.inUseBy", { count: usedBy });
}

interface DeleteConfigDialogProps {
  envId: string;
  // The config to delete, or null while the dialog is closed.
  config: { id: string; name: string } | null;
  onClose: () => void;
  // Called once the config is gone, after the dialog asked to close.
  onDeleted?: () => void;
}

// DeleteConfigDialog is "Delete {name}?" (UI-SPEC "Destructive and dangerous actions", default
// 13): a plain confirm with no typed name, since a config's content can be copied out first. The
// server checks live that no service spec mentions the config. Focus starts on Cancel, the dialog
// cannot be dismissed while the request is out, and a refusal stays inside it with the way
// forward. A config that is already gone is said so in a toast and the dialog closes, since the
// refreshed list no longer shows it.
export function DeleteConfigDialog({ envId, config, onClose, onDeleted }: DeleteConfigDialogProps) {
  const t = useT();
  const mutation = useDeleteConfig(envId);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const name = config?.name ?? "";

  function wording(err: ConfigRequestError): string {
    switch (err.type) {
      case "object_in_use":
        return t("configs.delete.inUse", { name });
      case "host_read_only":
        return t("configs.errors.hostReadOnly");
      case "no_manager_connected":
        return t("configs.errors.noManager");
      case "not_a_swarm":
        return t("configs.errors.notASwarm");
      default:
        return err.status === 403 ? t("configs.errors.permission") : t("configs.errors.generic");
    }
  }

  function close() {
    setError(null);
    onClose();
  }

  async function remove() {
    if (!config || inFlight.current) return;
    inFlight.current = true;
    setError(null);
    try {
      await mutation.mutateAsync({ id: config.id, name: config.name });
      toastSuccess(t("configs.delete.done", { name }));
      close();
      onDeleted?.();
    } catch (e) {
      const err = e instanceof ConfigRequestError ? e : new ConfigRequestError(0);
      if (err.type === "config_not_found") {
        toastInfo(t("configs.delete.gone", { name }));
        close();
      } else {
        setError(wording(err));
      }
    } finally {
      inFlight.current = false;
    }
  }

  function change(open: boolean) {
    if (open || mutation.isPending) return;
    close();
  }

  return (
    <AlertDialog open={config !== null} onOpenChange={change}>
      <AlertDialogContent className="sm:max-w-120">
        <AlertDialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-words">
            {t("configs.delete.title", { name })}
          </AlertDialogTitle>
          <AlertDialogDescription>{t("configs.delete.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={mutation.isPending} className={TOUCH_HEIGHT}>
            {t("configs.delete.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            aria-disabled={mutation.isPending}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90 aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={() => void remove()}
          >
            {mutation.isPending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("configs.delete.pending")}
              </>
            ) : (
              t("configs.delete.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
